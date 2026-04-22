// Package usbraw accede directamente al device USB de la impresora usando
// SetupDi API + overlapped I/O con timeout (evita bloqueo indefinido en ReadFile).
// Esto permite lectura bidireccional sin pasar por el spooler de Windows.
package usbraw

import (
	"bytes"
	"fmt"
	"log"
	"syscall"
	"time"
	"unsafe"
)

var (
	setupapi = syscall.NewLazyDLL("setupapi.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procSetupDiGetClassDevsW             = setupapi.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceInterfaces      = setupapi.NewProc("SetupDiEnumDeviceInterfaces")
	procSetupDiGetDeviceInterfaceDetailW = setupapi.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procSetupDiDestroyDeviceInfoList     = setupapi.NewProc("SetupDiDestroyDeviceInfoList")

	procCreateFileW         = kernel32.NewProc("CreateFileW")
	procWriteFile           = kernel32.NewProc("WriteFile")
	procReadFile            = kernel32.NewProc("ReadFile")
	procCloseHandle         = kernel32.NewProc("CloseHandle")
	procCreateEventW        = kernel32.NewProc("CreateEventW")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
	procGetOverlappedResult = kernel32.NewProc("GetOverlappedResult")
	procCancelIo            = kernel32.NewProc("CancelIo")
)

const (
	digcfPresent         = 0x00000002
	digcfDeviceInterface = 0x00000010

	genericRead        = 0x80000000
	genericWrite       = 0x40000000
	fileShareRead      = 0x00000001
	fileShareWrite     = 0x00000002
	openExisting       = 3
	fileFlagOverlapped = 0x40000000

	waitObject0  = 0x00000000
	waitTimeout  = 0x00000102
	errIoPending = syscall.Errno(997)
)

// GUID_DEVINTERFACE_USB_PRINT = {28d78fad-5a12-11d1-ae5b-0000f803a8c2}
var guidUSBPrint = winGUID{
	Data1: 0x28d78fad,
	Data2: 0x5a12,
	Data3: 0x11d1,
	Data4: [8]byte{0xae, 0x5b, 0x00, 0x00, 0xf8, 0x03, 0xa8, 0xc2},
}

type winGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

type spDeviceInterfaceData struct {
	cbSize             uint32
	InterfaceClassGuid winGUID
	Flags              uint32
	Reserved           uintptr
}

// overlapped corresponde a la estructura OVERLAPPED de Win32
type overlapped struct {
	Internal     uintptr
	InternalHigh uintptr
	Offset       uint32
	OffsetHigh   uint32
	HEvent       syscall.Handle
}

// FindDevicePaths devuelve los device paths de impresoras USB detectadas via SetupDi.
func FindDevicePaths() ([]string, error) {
	hDevInfo, _, e := procSetupDiGetClassDevsW.Call(
		uintptr(unsafe.Pointer(&guidUSBPrint)),
		0, 0,
		uintptr(digcfPresent|digcfDeviceInterface),
	)
	if hDevInfo == ^uintptr(0) {
		return nil, fmt.Errorf("SetupDiGetClassDevs: %w", e)
	}
	defer procSetupDiDestroyDeviceInfoList.Call(hDevInfo)

	var paths []string

	for i := uint32(0); i < 32; i++ {
		devIntfData := spDeviceInterfaceData{}
		devIntfData.cbSize = uint32(unsafe.Sizeof(devIntfData))

		r, _, _ := procSetupDiEnumDeviceInterfaces.Call(
			hDevInfo, 0,
			uintptr(unsafe.Pointer(&guidUSBPrint)),
			uintptr(i),
			uintptr(unsafe.Pointer(&devIntfData)),
		)
		if r == 0 {
			break
		}

		var requiredSize uint32
		procSetupDiGetDeviceInterfaceDetailW.Call(
			hDevInfo,
			uintptr(unsafe.Pointer(&devIntfData)),
			0, 0,
			uintptr(unsafe.Pointer(&requiredSize)),
			0,
		)
		if requiredSize < 8 {
			continue
		}

		buf := make([]uint16, requiredSize/2+2)
		cbSize := uint32(6)
		if unsafe.Sizeof(uintptr(0)) == 8 {
			cbSize = 8
		}
		*(*uint32)(unsafe.Pointer(&buf[0])) = cbSize

		r, _, _ = procSetupDiGetDeviceInterfaceDetailW.Call(
			hDevInfo,
			uintptr(unsafe.Pointer(&devIntfData)),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(requiredSize),
			uintptr(unsafe.Pointer(&requiredSize)),
			0,
		)
		if r == 0 {
			continue
		}

		// El device path empieza 4 bytes después del cbSize
		pathStart := buf[2:]
		path := syscall.UTF16ToString(pathStart)
		if path != "" {
			paths = append(paths, path)
			log.Printf("[USB-Raw] Device encontrado: %s", path)
		}
	}

	return paths, nil
}

// SendPJLAndRead abre el device USB con overlapped I/O, envía PJL y lee respuesta con timeout.
func SendPJLAndRead(devicePath, pjlCmd string, timeoutMs int) ([]byte, error) {
	pathW, err := syscall.UTF16PtrFromString(devicePath)
	if err != nil {
		return nil, err
	}

	// Abrir con FILE_FLAG_OVERLAPPED para poder usar timeout en lectura
	h, _, e := procCreateFileW.Call(
		uintptr(unsafe.Pointer(pathW)),
		uintptr(genericRead|genericWrite),
		uintptr(fileShareRead|fileShareWrite),
		0,
		uintptr(openExisting),
		uintptr(fileFlagOverlapped),
		0,
	)
	if h == ^uintptr(0) {
		return nil, fmt.Errorf("CreateFile: %w", e)
	}
	defer procCloseHandle.Call(h)

	log.Printf("[USB-Raw] Device abierto: %s", devicePath)

	// ── Escritura (también overlapped) ────────────────────────────────────
	wEvent, _, _ := procCreateEventW.Call(0, 1, 0, 0)
	if wEvent == 0 {
		return nil, fmt.Errorf("CreateEvent(write)")
	}
	defer procCloseHandle.Call(wEvent)

	wOv := overlapped{HEvent: syscall.Handle(wEvent)}
	cmdBytes := []byte(pjlCmd)
	var written uint32

	r, _, e := procWriteFile.Call(
		h,
		uintptr(unsafe.Pointer(&cmdBytes[0])),
		uintptr(len(cmdBytes)),
		uintptr(unsafe.Pointer(&written)),
		uintptr(unsafe.Pointer(&wOv)),
	)
	if r == 0 {
		if errno, ok := e.(syscall.Errno); ok && errno == errIoPending {
			procWaitForSingleObject.Call(wEvent, 5000)
			procGetOverlappedResult.Call(h, uintptr(unsafe.Pointer(&wOv)), uintptr(unsafe.Pointer(&written)), 0)
		} else {
			return nil, fmt.Errorf("WriteFile: %w", e)
		}
	}
	log.Printf("[USB-Raw] %d bytes escritos", written)

	// Esperar que la impresora procese
	wait := time.Duration(timeoutMs) * time.Millisecond
	if wait < 2*time.Second {
		wait = 2 * time.Second
	}
	time.Sleep(wait)

	// ── Lectura con timeout via overlapped I/O ────────────────────────────
	var result bytes.Buffer
	buf := make([]byte, 4096)

	first := true
	for {
		rEvent, _, _ := procCreateEventW.Call(0, 1, 0, 0)
		if rEvent == 0 {
			break
		}

		rOv := overlapped{HEvent: syscall.Handle(rEvent)}
		var readBytes uint32

		r, _, e = procReadFile.Call(
			h,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf)),
			uintptr(unsafe.Pointer(&readBytes)),
			uintptr(unsafe.Pointer(&rOv)),
		)

		if r != 0 {
			// Completó sincrónicamente
			procCloseHandle.Call(rEvent)
			if readBytes > 0 {
				result.Write(buf[:readBytes])
				first = false
				continue
			}
			break
		}

		errno, ok := e.(syscall.Errno)
		if !ok || errno != errIoPending {
			procCloseHandle.Call(rEvent)
			break
		}

		// Esperar
		toWait := uintptr(500)
		if first {
			toWait = 2500
		}
		waitResult, _, _ := procWaitForSingleObject.Call(rEvent, toWait)
		
		if waitResult == waitTimeout {
			procCancelIo.Call(h)
			procCloseHandle.Call(rEvent)
			break
		}

		if waitResult == waitObject0 {
			r2, _, _ := procGetOverlappedResult.Call(
				h,
				uintptr(unsafe.Pointer(&rOv)),
				uintptr(unsafe.Pointer(&readBytes)),
				0,
			)
			procCloseHandle.Call(rEvent)
			if r2 != 0 && readBytes > 0 {
				result.Write(buf[:readBytes])
				first = false
				continue
			}
		} else {
			procCloseHandle.Call(rEvent)
		}
		break
	}

	data := result.Bytes()
	if len(data) > 0 {
		DumpHex("Respuesta USB raw", data)
	}
	return data, nil
}

// DumpHex imprime bytes en hex+ASCII para diagnóstico.
func DumpHex(label string, data []byte) {
	log.Printf("[USB-Raw] %s (%d bytes):", label, len(data))
	for i := 0; i < len(data); i += 16 {
		end := i + 16
		if end > len(data) {
			end = len(data)
		}
		chunk := data[i:end]
		hexStr := ""
		ascii := ""
		for _, b := range chunk {
			hexStr += fmt.Sprintf("%02x ", b)
			if b >= 0x20 && b < 0x7e {
				ascii += string(rune(b))
			} else {
				ascii += "."
			}
		}
		log.Printf("  %04x  %-48s  %s", i, hexStr, ascii)
	}
}
