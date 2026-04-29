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
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
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
					Name:       p.PropertyName,
					Color:      "black",
					Type:       "toner",
					Percentage: payload.Float64Ptr(float64(lvl)),
					Status:     supplyStatus(lvl),
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
// Usa IBidiSpl2 (bidispl.dll, Windows 8+): interfaz XML más simple que IBidiSpl v1.
// CLSIDs registrados en HKLM\SOFTWARE\Classes\CLSID como "Bidi Spooler APIs":
//   {2A614240-A4C5-4C33-BD87-1BC709331639} → InprocServer32 = bidispl.dll
//   {B9162A23-45F9-47CC-80F5-FE0FE9B9E1A2}
//   {FC5B8A24-DB05-4A01-8388-22EDF6C2BBBA}
// IID_IBidiSpl2 = {D9B3B463-DEF7-4C56-B61A-8CAE77EB3ABD} (bidispl.h, Windows 10 SDK)
const csharpBidi = `
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using System.Text;
using System.Xml;

// IBidiSpl2: interfaz moderna (Windows 8+) con BindDevice + SendRecvXMLString
[ComImport, Guid("D9B3B463-DEF7-4C56-B61A-8CAE77EB3ABD"),
 InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
public interface IBidiSpl2 {
    [PreserveSig] int BindDevice([MarshalAs(UnmanagedType.LPWStr)] string pszDeviceName, uint dwAccess);
    [PreserveSig] int UnbindDevice();
    [PreserveSig] int SendRecvXMLString(
        [MarshalAs(UnmanagedType.BStr)] string bstrRequest,
        [MarshalAs(UnmanagedType.BStr)] out string pbstrResponse);
    [PreserveSig] int SendRecvXMLStream(IntPtr pSRequest, out IntPtr ppSResponse);
}

public static class BidiExtractor {
    const uint BIDI_ACCESS_USER = 0x00000001;
    const string BidiNs = "http://schemas.microsoft.com/windows/2005/03/printing/bidi";

    static readonly string[] Schemas = {
        @"\Printer.Supplies#1.Level",
        @"\Printer.Supplies#1.MaxCapacity",
        @"\Printer.Supplies#1.ColorantName",
        @"\Printer.Supplies#2.Level",
        @"\Printer.Supplies#2.MaxCapacity",
        @"\Printer.Supplies#2.ColorantName",
        @"\Printer.PageCount.Value",
        @"\Printer.DeviceInfo:SerialNumber",
        @"\Printer.Status.Summary",
    };

    // Intenta instanciar IBidiSpl2 probando los tres CLSIDs registrados ("Bidi Spooler APIs").
    // Nota: estos CLSIDs (bidispl.dll) son proxies del spooler; si ninguno implementa
    // IBidiSpl2, es porque el acceso requiere contexto interno del print spooler.
    static IBidiSpl2 CreateBidiSpl2() {
        var clsids = new[] {
            new Guid("2A614240-A4C5-4C33-BD87-1BC709331639"),
            new Guid("B9162A23-45F9-47CC-80F5-FE0FE9B9E1A2"),
            new Guid("FC5B8A24-DB05-4A01-8388-22EDF6C2BBBA"),
        };
        var tried = new System.Collections.Generic.List<string>();
        foreach (var clsid in clsids) {
            try {
                var t = Type.GetTypeFromCLSID(clsid, true);
                var obj = Activator.CreateInstance(t);
                var spl = obj as IBidiSpl2;
                if (spl != null) return spl;
                tried.Add(clsid.ToString() + ":E_NOINTERFACE");
            } catch (Exception ex) { tried.Add(clsid.ToString() + ":" + ex.HResult.ToString("X")); }
        }
        throw new Exception("IBidiSpl2 no accesible (CLSIDs probados: " + string.Join(", ", tried) + ")");
    }

    public static string Run(string printerName) {
        IBidiSpl2 spl;
        try {
            spl = CreateBidiSpl2();
        } catch (Exception ex) {
            return "{\"_error\":\"CreateBidiSpl2: " + Esc(ex.Message) + "\"}";
        }

        int hr = spl.BindDevice(printerName, BIDI_ACCESS_USER);
        if (hr < 0) {
            return "{\"_error\":\"BindDevice HRESULT=0x" + hr.ToString("X8") + "\"}";
        }

        // Construir petición XML con todos los schemas
        var xmlReq = new StringBuilder();
        xmlReq.Append("<bidi:Get xmlns:bidi=\"").Append(BidiNs).Append("\">");
        foreach (var s in Schemas)
            xmlReq.Append("<Query schema=\"").Append(s).Append("\"/>");
        xmlReq.Append("</bidi:Get>");

        string xmlResp = null;
        hr = spl.SendRecvXMLString(xmlReq.ToString(), out xmlResp);
        spl.UnbindDevice();

        if (hr < 0 || string.IsNullOrEmpty(xmlResp)) {
            return "{\"_error\":\"SendRecvXMLString HRESULT=0x" + hr.ToString("X8") + "\"}";
        }

        // Parsear respuesta XML (C# 4 compatible: sin ?. ni string interpolation)
        var results = new Dictionary<string, string>();
        try {
            var doc = new XmlDocument();
            doc.LoadXml(xmlResp);
            var mgr = new XmlNamespaceManager(doc.NameTable);
            mgr.AddNamespace("bidi", BidiNs);
            XmlNodeList nodes = doc.SelectNodes("//Query[@schema]", mgr);
            foreach (XmlNode query in nodes) {
                XmlAttribute schemaAttr = query.Attributes["schema"];
                string schema = (schemaAttr != null) ? schemaAttr.Value : "";
                string val = "";
                foreach (XmlNode child in query.ChildNodes) {
                    string t = (child.InnerText != null) ? child.InnerText.Trim() : "";
                    if (t != "") { val = t; break; }
                }
                if (schema != "" && val != "" && val != "?")
                    results[schema] = val;
            }
        } catch {}

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
		res.Supplies = append(res.Supplies, payload.Supply{
			Name:       supplyName + " Toner",
			Color:      strings.ToLower(supplyName),
			Type:       "toner",
			Percentage: payload.Float64Ptr(float64(level)),
			Status:     supplyStatus(level),
		})
	}

	return res, nil
}

func supplyStatus(pct int) string {
	switch {
	case pct <= 10:
		return "Cr\u00edtico"
	case pct <= 25:
		return "Bajo"
	default:
		return "OK"
	}
}
