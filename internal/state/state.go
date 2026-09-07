package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"usb-agent/internal/payload"
)

type PrinterState struct {
	LastPollAt string                      `json:"last_poll_at"`
	Counters   SavedCountersState          `json:"counters"`
	Supplies   map[string]SavedSupplyState `json:"supplies"`
}

type SavedCountersState struct {
	TotalPages        int64 `json:"total_pages"`
	MonoPages         int64 `json:"mono_pages"`
	ColorPages        int64 `json:"color_pages"`
	ScanPages         int64 `json:"scan_pages"`
	CopyPages         int64 `json:"copy_pages"`
	FaxPages          int64 `json:"fax_pages"`
	SimplexPages      int64 `json:"simplex_pages"`
	EngineCycles      int64 `json:"engine_cycles"`
	EngineCyclesMono  int64 `json:"engine_cycles_mono"`
	EngineCyclesColor int64 `json:"engine_cycles_color"`
}

type SavedSupplyState struct {
	RawLevel         int   `json:"raw_level"`
	PagesAtInstall   int64 `json:"pages_at_install"`
	ChangeCount      int   `json:"change_count"`
	TrackedFromStart bool  `json:"tracked_from_start"`
}

func GetStateFilename(stateDir, printerID string) string {
	return filepath.Join(stateDir, fmt.Sprintf("printer_%s.json", printerID))
}

func LoadState(stateDir, printerID string) (*PrinterState, error) {
	path := GetStateFilename(stateDir, printerID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // First time
		}
		return nil, err
	}
	var s PrinterState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func SaveState(stateDir, printerID string, p *payload.Payload) error {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return err
	}
	s := PrinterState{
		LastPollAt: time.Now().UTC().Format(time.RFC3339),
	}
	if p.Counters.Absolute.Total != nil {
		s.Counters.TotalPages = *p.Counters.Absolute.Total
	}
	if p.Counters.Absolute.Mono != nil {
		s.Counters.MonoPages = *p.Counters.Absolute.Mono
	}
	if p.Counters.Absolute.Color != nil {
		s.Counters.ColorPages = *p.Counters.Absolute.Color
	}
	if p.Counters.HardwareUsage.TotalScans != nil {
		s.Counters.ScanPages = *p.Counters.HardwareUsage.TotalScans
	}
	if p.Counters.LogicalMatrix.ByFunction.Copy != nil {
		s.Counters.CopyPages = *p.Counters.LogicalMatrix.ByFunction.Copy
	}
	if p.Counters.LogicalMatrix.ByFunction.FaxPrint != nil {
		s.Counters.FaxPages = *p.Counters.LogicalMatrix.ByFunction.FaxPrint
	}
	if p.Counters.LogicalMatrix.ByMode.Simplex != nil {
		s.Counters.SimplexPages = *p.Counters.LogicalMatrix.ByMode.Simplex
	}
	if p.Counters.HardwareUsage.EngineCycles != nil {
		s.Counters.EngineCycles = *p.Counters.HardwareUsage.EngineCycles
	}

	// Restaurado: Guardar estado de suministros para el tracking predictivo
	s.Supplies = make(map[string]SavedSupplyState)
	for _, sup := range p.Supplies {
		raw := 0
		if sup.RawLevel != nil {
			raw = *sup.RawLevel
		}

		pagesAtInstall := int64(0)
		if p.Counters.Absolute.Total != nil {
			if sup.PagesWithSupply != nil {
				pagesAtInstall = *p.Counters.Absolute.Total - int64(*sup.PagesWithSupply)
			} else {
				pagesAtInstall = *p.Counters.Absolute.Total
			}
		}

		changes := 0
		if sup.ChangeCount != nil {
			changes = *sup.ChangeCount
		}

		tracked := false
		// Intentar mantener el trackeo previo si existía en la ejecución actual
		// (Para no perder TrackedFromStart al sobreescribir el state)
		if sup.PagesWithSupply != nil {
			tracked = true
		}

		s.Supplies[sup.ID] = SavedSupplyState{
			RawLevel:         raw,
			PagesAtInstall:   pagesAtInstall,
			ChangeCount:      changes,
			TrackedFromStart: tracked,
		}
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := GetStateFilename(stateDir, printerID)
	return os.WriteFile(path, data, 0644)
}

func CalculateDelta(prev *PrinterState, curr *payload.Payload) (*payload.CountersDelta, bool) {
	if prev == nil {
		return nil, false
	}

	currentCounters := SavedCountersState{}
	if curr.Counters.Absolute.Total != nil {
		currentCounters.TotalPages = *curr.Counters.Absolute.Total
	}
	if curr.Counters.Absolute.Mono != nil {
		currentCounters.MonoPages = *curr.Counters.Absolute.Mono
	}
	if curr.Counters.Absolute.Color != nil {
		currentCounters.ColorPages = *curr.Counters.Absolute.Color
	}
	if curr.Counters.HardwareUsage.TotalScans != nil {
		currentCounters.ScanPages = *curr.Counters.HardwareUsage.TotalScans
	}
	if curr.Counters.LogicalMatrix.ByFunction.Copy != nil {
		currentCounters.CopyPages = *curr.Counters.LogicalMatrix.ByFunction.Copy
	}
	if curr.Counters.LogicalMatrix.ByFunction.FaxPrint != nil {
		currentCounters.FaxPages = *curr.Counters.LogicalMatrix.ByFunction.FaxPrint
	}
	if curr.Counters.LogicalMatrix.ByMode.Simplex != nil {
		currentCounters.SimplexPages = *curr.Counters.LogicalMatrix.ByMode.Simplex
	}
	if curr.Counters.HardwareUsage.EngineCycles != nil {
		currentCounters.EngineCycles = *curr.Counters.HardwareUsage.EngineCycles
	}

	for i, s := range curr.Supplies {
		if prevSup, ok := prev.Supplies[s.ID]; ok {
			// El usuario solicitó remover el algoritmo de change_count y pages_with_supply 
			// para evitar falsos positivos por fluctuaciones menores de los sensores.
			curr.Supplies[i].PagesWithSupply = nil
			curr.Supplies[i].ChangeCount = nil
			_ = prevSup // Silenciar error de variable no usada
		}
	}

	isReset := false
	if currentCounters.TotalPages < prev.Counters.TotalPages ||
		currentCounters.MonoPages < prev.Counters.MonoPages ||
		currentCounters.ColorPages < prev.Counters.ColorPages ||
		currentCounters.ScanPages < prev.Counters.ScanPages ||
		currentCounters.CopyPages < prev.Counters.CopyPages ||
		currentCounters.FaxPages < prev.Counters.FaxPages ||
		currentCounters.SimplexPages < prev.Counters.SimplexPages ||
		currentCounters.EngineCycles < prev.Counters.EngineCycles {
		isReset = true
	}

	delta := &payload.CountersDelta{}

	if isReset {
		delta.TotalPages = payload.Int64Ptr(0)
		delta.MonoPages = payload.Int64Ptr(0)
		delta.ColorPages = payload.Int64Ptr(0)
		delta.ScanPages = payload.Int64Ptr(0)
		delta.CopyPages = payload.Int64Ptr(0)
		delta.FaxPages = nil
		delta.SimplexPages = payload.Int64Ptr(0)
		delta.EngineCycles = payload.Int64Ptr(0)
		delta.EngineCyclesMono = nil
		delta.EngineCyclesColor = nil
		return delta, true
	}

	delta.TotalPages = payload.Int64Ptr(maxInt64(0, currentCounters.TotalPages-prev.Counters.TotalPages))
	delta.MonoPages = payload.Int64Ptr(maxInt64(0, currentCounters.MonoPages-prev.Counters.MonoPages))
	delta.ColorPages = payload.Int64Ptr(maxInt64(0, currentCounters.ColorPages-prev.Counters.ColorPages))
	delta.ScanPages = payload.Int64Ptr(maxInt64(0, currentCounters.ScanPages-prev.Counters.ScanPages))
	delta.CopyPages = payload.Int64Ptr(maxInt64(0, currentCounters.CopyPages-prev.Counters.CopyPages))
	delta.FaxPages = nil
	delta.SimplexPages = payload.Int64Ptr(maxInt64(0, currentCounters.SimplexPages-prev.Counters.SimplexPages))
	delta.EngineCycles = payload.Int64Ptr(maxInt64(0, currentCounters.EngineCycles-prev.Counters.EngineCycles))
	delta.EngineCyclesMono = nil
	delta.EngineCyclesColor = nil

	return delta, false
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
