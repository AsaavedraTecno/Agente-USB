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
	// ERROR_NO_DATA (232): USB printer driver returns this immediately when the
	// bulk-IN endpoint has no data queued yet, instead of keeping the read pending.
	errNoData = syscall.Errno(232)
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
// Devuelve (respuesta fresca, bytes drenados del buffer previo, error).
// Si la lectura fresca falla, el llamador puede usar los bytes drenados como fallback —
// son la respuesta válida de la ejecución anterior (la impresora responde ~5-7 s después
// de recibir el comando, más de lo que espera el driver en el primer intento).
func SendPJLAndRead(devicePath, pjlCmd string, timeoutMs int, isComplete func(data []byte, isIdle bool) bool) (fresh []byte, drained []byte, err error) {
	pathW, convErr := syscall.UTF16PtrFromString(devicePath)
	if convErr != nil {
		return nil, nil, convErr
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
		return nil, nil, fmt.Errorf("CreateFile: %w", e)
	}
	defer procCloseHandle.Call(h)

	log.Printf("[USB-Raw] Device abierto: %s", devicePath)

	// Drain stale data from previous queries before sending.
	// The Brother HL-L5xxx takes ~5-7 s to build its full response after receiving
	// a PJL command.  The USB driver returns ERROR_NO_DATA immediately, so the
	// previous run's read times out before the data arrives.  The response then sits
	// in the USB bulk-IN buffer until the next run drains it here.
	// We capture those bytes so the caller can use them as a fallback instead of
	// discarding perfectly valid data from the previous cycle.
	{
		var drainBuf bytes.Buffer
		dbuf := make([]byte, 65536)
		for {
			de, _, _ := procCreateEventW.Call(0, 1, 0, 0)
			if de == 0 {
				break
			}
			dov := overlapped{HEvent: syscall.Handle(de)}
			var dn uint32
			dr, _, derr := procReadFile.Call(h, uintptr(unsafe.Pointer(&dbuf[0])), uintptr(len(dbuf)),
				uintptr(unsafe.Pointer(&dn)), uintptr(unsafe.Pointer(&dov)))
			if dr == 0 {
				derrno, ok := derr.(syscall.Errno)
				if ok && derrno == errIoPending {
					wr, _, _ := procWaitForSingleObject.Call(de, 200)
					if wr == waitTimeout {
						procCancelIo.Call(h)
						procCloseHandle.Call(de)
						break // nothing arrived in 200 ms — buffer is clean
					}
					procGetOverlappedResult.Call(h, uintptr(unsafe.Pointer(&dov)), uintptr(unsafe.Pointer(&dn)), 0)
				} else {
					// errNoData (232) or any other error means the buffer is empty.
					procCloseHandle.Call(de)
					break
				}
			}
			procCloseHandle.Call(de)
			if dn == 0 {
				break
			}
			drainBuf.Write(dbuf[:dn])
		}
		drained = drainBuf.Bytes()
		if len(drained) > 0 {
			log.Printf("[USB-Raw] Buffer previo: %d bytes capturados (respuesta del ciclo anterior)", len(drained))
		}
	}

	// ── Escritura (también overlapped) ────────────────────────────────────
	wEvent, _, _ := procCreateEventW.Call(0, 1, 0, 0)
	if wEvent == 0 {
		return nil, drained, fmt.Errorf("CreateEvent(write)")
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
			return nil, drained, fmt.Errorf("WriteFile: %w", e)
		}
	}
	log.Printf("[USB-Raw] %d bytes escritos", written)

	// Brief initial pause; the real wait is handled in the read loop below.
	time.Sleep(1 * time.Second)

	// ── Lectura con deadline de pared ────────────────────────────────────────
	// Usando un deadline absoluto en vez de contadores de reintentos.
	// Esto resuelve el problema de timing: cuando el spooler está detenido, el driver
	// devuelve errIoPending (en vez de ERROR_NO_DATA) en lecturas vacías, por lo que
	// el contador de reintentos no aplica. Con un deadline, ambos casos quedan cubiertos:
	//   - ERROR_NO_DATA: duerme 1s y vuelve si hay tiempo restante
	//   - errIoPending:  WaitForSingleObject usa el tiempo restante del deadline
	// Con timeoutMs=30000 (bypass): ventana de 30s captura BRSUPPLY (~8-10s tras write).
	// Con timeoutMs=5000 (normal):  ventana de 5s captura solo ID+STATUS.
	readDeadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)

	var result bytes.Buffer
	buf := make([]byte, 4096)

	for time.Now().Before(readDeadline) {
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
			// Completed synchronously.
			procCloseHandle.Call(rEvent)
			if readBytes > 0 {
				result.Write(buf[:readBytes])
				if isComplete != nil && isComplete(result.Bytes(), false) {
					break
				}
				continue
			}
			// Sync 0-byte: buffer vacío, pausar muy brevemente para no saturar CPU.
			if isComplete != nil && isComplete(result.Bytes(), true) {
				break
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}

		errno, ok := e.(syscall.Errno)
		if !ok {
			log.Printf("[USB-Raw] ReadFile: error no convertible (%v)", e)
			procCloseHandle.Call(rEvent)
			break
		}

		// ERROR_NO_DATA (232): buffer vacío. Reintentar casi inmediato para no perder datos.
		if errno == errNoData {
			procCloseHandle.Call(rEvent)
			if isComplete != nil && isComplete(result.Bytes(), true) {
				break
			}
			remaining := time.Until(readDeadline)
			if remaining <= 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}

		if errno != errIoPending {
			log.Printf("[USB-Raw] ReadFile error inesperado: errno=%d", errno)
			procCloseHandle.Call(rEvent)
			break
		}

		// errIoPending: driver mantiene la lectura pendiente (spooler detenido o buffer llenándose).
		// Esperar hasta el deadline.
		remaining := time.Until(readDeadline)
		if remaining < 200*time.Millisecond {
			procCancelIo.Call(h)
			procWaitForSingleObject.Call(rEvent, 500)
			procCloseHandle.Call(rEvent)
			break
		}
		toWait := uintptr(remaining.Milliseconds())
		if toWait > 30000 {
			toWait = 30000
		}

		waitResult, _, _ := procWaitForSingleObject.Call(rEvent, toWait)

		if waitResult == waitTimeout {
			procCancelIo.Call(h)
			procWaitForSingleObject.Call(rEvent, 500)
			procCloseHandle.Call(rEvent)
			break
		}

		if waitResult == waitObject0 {
			r2, _, err2 := procGetOverlappedResult.Call(
				h,
				uintptr(unsafe.Pointer(&rOv)),
				uintptr(unsafe.Pointer(&readBytes)),
				0,
			)
			procCloseHandle.Call(rEvent)
			
			// Si leímos datos, conservarlos incluso si r2 == 0 (ej. ERROR_MORE_DATA)
			if readBytes > 0 {
				result.Write(buf[:readBytes])
				if isComplete != nil && isComplete(result.Bytes(), false) {
					break
				}
				continue
			}
			
			if r2 != 0 {
				// 0-byte packet (ZLP). Pause briefly.
				if isComplete != nil && isComplete(result.Bytes(), true) {
					break
				}
				time.Sleep(50 * time.Millisecond)
				continue
			} else if err2 != nil && err2.Error() != "The operation completed successfully." {
				// Si falló y no leímos nada, loguear para diagnosticar
				log.Printf("[USB-Raw] OverlappedResult error: %v", err2)
			}

		} else {
			procCloseHandle.Call(rEvent)
		}
		break
	}

	fresh = result.Bytes()
	if len(fresh) > 0 {
		DumpHex("Respuesta USB raw (fresca)", fresh)
	}
	return fresh, drained, nil
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
