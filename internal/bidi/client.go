// Package bidi extrae telemetría de impresora via la Windows Bidirectional Communication API.
// Prueba dos métodos:
//  1. Get-PrinterProperty (PowerShell nativo, sin compilación)
//  2. IBidiSpl COM via C# compilado en memoria con Add-Type
//
// Diseñado como complemento al PJL: rellena tóner/páginas cuando PJL devuelve "?".
package bidi

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"usb-agent/internal/payload"
)

// Result contiene la telemetría extraída vía Bidi.
type Result struct {
	PageCount int64
	Serial    string
	Supplies  []payload.Supply
}

// Extract intenta obtener telemetría usando la Windows Bidi API.
// Prueba Get-PrinterProperty primero; si no hay datos útiles, usa IBidiSpl COM.
func Extract(printerName string) (*Result, error) {
	if res, err := tryGetPrinterProperty(printerName); err == nil {
		return res, nil
	}
	return tryBidiCOM(printerName)
}

// ── Método 1: Get-PrinterProperty ─────────────────────────────────────────────

func tryGetPrinterProperty(printerName string) (*Result, error) {
	safe := strings.ReplaceAll(printerName, "'", "''")
	script := fmt.Sprintf(`
$p = Get-PrinterProperty -PrinterName '%s' -ErrorAction SilentlyContinue
if (-not $p) { Write-Output 'null'; exit }
$p | Select-Object PropertyName, @{N='Value';E={[string]$_.Value}} | ConvertTo-Json -Compress`, safe)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	psCmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	psCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := psCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("Get-PrinterProperty: %w", err)
	}

	raw := strings.TrimSpace(string(out))
	if raw == "" || raw == "null" {
		return nil, fmt.Errorf("Get-PrinterProperty: sin datos")
	}

	return parsePrinterProperties(raw)
}

func parsePrinterProperties(raw string) (*Result, error) {
	type prop struct {
		PropertyName string `json:"PropertyName"`
		Value        string `json:"Value"`
	}

	var props []prop
	if strings.HasPrefix(raw, "[") {
		if err := json.Unmarshal([]byte(raw), &props); err != nil {
			return nil, fmt.Errorf("parsear Get-PrinterProperty: %w", err)
		}
	} else {
		var single prop
		if err := json.Unmarshal([]byte(raw), &single); err != nil {
			return nil, fmt.Errorf("parsear Get-PrinterProperty (single): %w", err)
		}
		props = []prop{single}
	}

	res := &Result{}
	var cartridgeModel *string
	var cartridgeSerial *string

	// First pass: look for global cartridge identifiers
	for _, p := range props {
		name := strings.ToLower(p.PropertyName)
		val := strings.TrimSpace(p.Value)
		if val == "" || val == "?" {
			continue
		}
		if strings.Contains(name, "partnumber") || strings.Contains(name, "cartridgemodel") {
			m := val
			cartridgeModel = &m
		}
		if strings.Contains(name, "cartridgeserial") {
			s := val
			cartridgeSerial = &s
		}
	}

	for _, p := range props {
		name := strings.ToLower(p.PropertyName)
		val := strings.TrimSpace(p.Value)
		if val == "" || val == "?" {
			continue
		}
		switch {
		case strings.Contains(name, "pagecount") || strings.Contains(name, "totalpage") ||
			strings.Contains(name, "printcount"):
			fmt.Sscanf(val, "%d", &res.PageCount)
		case strings.Contains(name, "serial"):
			res.Serial = val
		case strings.Contains(name, "toner") || strings.Contains(name, "supply") ||
			strings.Contains(name, "consumable"):
			var lvl int
			if n, _ := fmt.Sscanf(val, "%d", &lvl); n == 1 && lvl >= 0 && lvl <= 100 {
				res.Supplies = append(res.Supplies, payload.Supply{
					Name:         p.PropertyName,
					Color:        "black",
					Type:         "toner",
					Category:     "toner",
					Percentage:   payload.Float64Ptr(float64(lvl)),
					Status:       supplyStatus(lvl),
					Model:        cartridgeModel,
					SerialNumber: cartridgeSerial,
				})
			}
		}
	}

	if res.PageCount == 0 && res.Serial == "" && len(res.Supplies) == 0 {
		return nil, fmt.Errorf("Get-PrinterProperty: sin datos útiles")
	}
	return res, nil
}

// ── Método 2: IBidiSpl COM via C# Add-Type ────────────────────────────────────

// csharpBidi es código C# compilado en memoria por PowerShell Add-Type.
// Usa IBidiSpl v1 (compatible con todos los Windows y drivers antiguos).
const csharpBidi = `
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using System.Text;

[ComImport, Guid("05121968-360B-4F8B-A36C-624D6F973686"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IBidiSpl {
    [PreserveSig] int BindDevice([MarshalAs(UnmanagedType.LPWStr)] string pszDeviceName, uint dwAccess);
    [PreserveSig] int UnbindDevice();
    [PreserveSig] int SendRecv([MarshalAs(UnmanagedType.LPWStr)] string pszAction, IBidiRequest pRequest, out IBidiRequest ppResponse);
    [PreserveSig] int MultiSendRecv([MarshalAs(UnmanagedType.LPWStr)] string pszAction, IntPtr pRequestContainer, out IntPtr ppResponseContainer);
}

[ComImport, Guid("D79C53E4-0E39-4328-B521-6F5D92C23C5E"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IBidiRequest {
    [PreserveSig] int SetSchema([MarshalAs(UnmanagedType.LPWStr)] string pszSchema);
    [PreserveSig] int SetInputData(uint dwType, IntPtr pData, uint uSize);
    [PreserveSig] int GetResult(out int phrValidData);
    [PreserveSig] int GetOutputData(uint dwIndex, [MarshalAs(UnmanagedType.LPWStr)] out string ppszSchema, out uint pdwType, out IntPtr ppData, out uint puSize);
    [PreserveSig] int GetEnumCount(out uint pdwTotal);
}

public static class BidiExtractor {
    const uint BIDI_ACCESS_USER = 0x00000001;

    static readonly string[] Schemas = {
        @"\Printer.Supplies#1.Level",
        @"\Printer.Supplies#1.MaxCapacity",
        @"\Printer.Supplies#1.ColorantName",
        @"\Printer.Supplies#1.PartNumber",
        @"\Printer.Supplies#1.ModelName",
        @"\Printer.Supplies#1.SerialNumber",
        @"\Printer.Supplies#2.Level",
        @"\Printer.Supplies#2.MaxCapacity",
        @"\Printer.Supplies#2.ColorantName",
        @"\Printer.Supplies#2.PartNumber",
        @"\Printer.Supplies#2.ModelName",
        @"\Printer.Supplies#2.SerialNumber",
        @"\Printer.PageCount.Value",
        @"\Printer.DeviceInfo:SerialNumber",
        @"\Printer.Status.Summary",
    };

    public static string Run(string printerName) {
        var clsSpl = new Guid("2A614240-A4C5-4C33-BD87-1BC709331639");
        var clsReq = new Guid("B9162A23-45F9-47CC-80F5-FE0FE9B9E1A2");

        IBidiSpl spl = null;
        try {
            spl = (IBidiSpl)Activator.CreateInstance(Type.GetTypeFromCLSID(clsSpl, true));
        } catch (Exception ex) {
            return "{\"_error\":\"CreateBidiSpl: " + Esc(ex.Message) + "\"}";
        }

        int hr = spl.BindDevice(printerName, BIDI_ACCESS_USER);
        if (hr < 0) {
            return "{\"_error\":\"BindDevice HRESULT=0x" + hr.ToString("X8") + "\"}";
        }

        var results = new Dictionary<string, string>();

        foreach (var schema in Schemas) {
            try {
                IBidiRequest req = (IBidiRequest)Activator.CreateInstance(Type.GetTypeFromCLSID(clsReq, true));
                req.SetSchema(schema);
                IBidiRequest resp;
                hr = spl.SendRecv("Get", req, out resp);
                if (hr >= 0 && resp != null) {
                    int valid = -1;
                    resp.GetResult(out valid);
                    if (valid == 0) { // 0 = S_OK
                        uint count = 0;
                        resp.GetEnumCount(out count);
                        for (uint i = 0; i < count; i++) {
                            string outSchema;
                            uint type;
                            IntPtr pData;
                            uint size;
                            if (resp.GetOutputData(i, out outSchema, out type, out pData, out size) == 0) {
                                string valStr = "";
                                if (type == 1) { // BIDI_INT
                                    valStr = Marshal.ReadInt32(pData).ToString();
                                } else if (type == 4 || type == 5 || type == 6) { // STRING, TEXT, ENUM
                                    valStr = Marshal.PtrToStringUni(pData);
                                } else if (type == 3) { // BOOL
                                    valStr = Marshal.ReadInt32(pData) == 0 ? "false" : "true";
                                }
                                if (!string.IsNullOrEmpty(outSchema) && !string.IsNullOrEmpty(valStr)) {
                                    results[outSchema] = valStr;
                                }
                            }
                        }
                    }
                }
            } catch {}
        }

        spl.UnbindDevice();

        var sb = new StringBuilder("{");
        bool first = true;
        foreach (var kv in results) {
            if (!first) sb.Append(",");
            sb.Append("\"").Append(Esc(kv.Key)).Append("\":\"").Append(Esc(kv.Value)).Append("\"");
            first = false;
        }
        sb.Append("}");
        return sb.ToString();
    }

    static string Esc(string s) {
        return s.Replace("\\","\\\\").Replace("\"","\\\"").Replace("\r","").Replace("\n"," ");
    }
}
`

func tryBidiCOM(printerName string) (*Result, error) {
	// Escapar nombre para insertar en string C# (ya dentro de la llamada PowerShell, no en el código C#)
	safeName := strings.ReplaceAll(printerName, `"`, `\"`)

	psScript := fmt.Sprintf(`
$code = @'
%s
'@
$err = $null
try {
    Add-Type -TypeDefinition $code -ReferencedAssemblies "System.Runtime.InteropServices","System.Xml" -ErrorAction Stop 2>$null
} catch { $err = $_.Exception.Message }
if ($err) { Write-Output ("{""_error"":""Add-Type: $($err -replace '""',""'"")""}" ) }
else { [BidiExtractor]::Run("%s") }`, csharpBidi, safeName)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", psScript)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("IBidiSpl COM: %w", err)
	}

	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, fmt.Errorf("IBidiSpl COM: sin respuesta")
	}

	var data map[string]string
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("IBidiSpl COM: parsear JSON (%q): %w", raw, err)
	}

	if errMsg, ok := data["_error"]; ok {
		return nil, fmt.Errorf("IBidiSpl COM: %s", errMsg)
	}

	res := &Result{}
	for k, v := range data {
		if v == "" || v == "?" {
			continue
		}
		switch {
		case strings.Contains(k, "PageCount"):
			fmt.Sscanf(v, "%d", &res.PageCount)
		case strings.Contains(k, "SerialNumber"):
			res.Serial = v
		}
	}

	// Construir Supplies con nivel + colorante
	for i := 1; i <= 2; i++ {
		lvlKey := fmt.Sprintf(`\Printer.Supplies#%d.Level`, i)
		nameKey := fmt.Sprintf(`\Printer.Supplies#%d.ColorantName`, i)
		pnKey := fmt.Sprintf(`\Printer.Supplies#%d.PartNumber`, i)
		mnKey := fmt.Sprintf(`\Printer.Supplies#%d.ModelName`, i)
		snKey := fmt.Sprintf(`\Printer.Supplies#%d.SerialNumber`, i)

		lvlStr, okL := data[lvlKey]
		if !okL {
			continue
		}
		var level int
		if n, _ := fmt.Sscanf(lvlStr, "%d", &level); n == 0 {
			continue
		}
		supplyName := data[nameKey]
		if supplyName == "" {
			supplyName = "Black"
		}

		var model *string
		if m := data[pnKey]; m != "" && m != "?" {
			model = &m
		} else if m := data[mnKey]; m != "" && m != "?" {
			model = &m
		}

		var serialNum *string
		if s := data[snKey]; s != "" && s != "?" {
			serialNum = &s
		}

		res.Supplies = append(res.Supplies, payload.Supply{
			Name:         supplyName + " Toner",
			Color:        strings.ToLower(supplyName),
			Type:         "toner",
			Category:     "toner",
			Percentage:   payload.Float64Ptr(float64(level)),
			Status:       supplyStatus(level),
			Model:        model,
			SerialNumber: serialNum,
		})
	}

	return res, nil
}

func supplyStatus(pct int) string {
	switch {
	case pct <= 10:
		return "cr\u00edtico"
	case pct <= 25:
		return "bajo"
	default:
		return "ok"
	}
}
