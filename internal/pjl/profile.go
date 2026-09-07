package pjl

import (
	"strings"
)

type BrandProfile interface {
	Name() string
	BuildPJLCommands() string
	ShouldParseSupplies() bool
	ShouldParseCounters() bool
	DetermineConfidence(hasSupplies, hasCounters bool) string
}

type BrotherProfile struct{}

func (p *BrotherProfile) Name() string { return "Brother" }
func (p *BrotherProfile) BuildPJLCommands() string {
	return uel + "@PJL" + crlf +
		"@PJL INFO ID" + crlf +
		"@PJL INFO STATUS" + crlf +
		"@PJL INFO SUPPLIES" + crlf +
		"@PJL INFO BRSUPPLY" + crlf +
		"@PJL INFO PRODINFO" + crlf +
		"@PJL INFO NETWORK" + crlf +
		"@PJL INFO BRNETINFO" + crlf +
		"@PJL INQUIRE IPADDRESS" + crlf +
		"@PJL INQUIRE IPV4" + crlf +
		"@PJL INQUIRE MACADDRESS" + crlf +
		"@PJL DINQUIRE PAGECOUNT" + crlf +
		"@PJL INQUIRE PAGECOUNT" + crlf +
		"@PJL INFO CONFIG" + crlf +
		"@PJL INFO VARIABLES" + crlf +
		uel
}
func (p *BrotherProfile) ShouldParseSupplies() bool { return true }
func (p *BrotherProfile) ShouldParseCounters() bool { return true }
func (p *BrotherProfile) DetermineConfidence(hasSupplies, hasCounters bool) string {
	if hasSupplies || hasCounters {
		return "pjl_full"
	}
	return "pjl_basic"
}

type HPProfileStandard struct{}

func (p *HPProfileStandard) Name() string { return "HP Standard" }
func (p *HPProfileStandard) BuildPJLCommands() string {
	return uel + "@PJL" + crlf +
		"@PJL INFO ID" + crlf +
		"@PJL INFO STATUS" + crlf +
		"@PJL INFO SUPPLIES" + crlf +
		"@PJL INFO PRODINFO" + crlf +
		"@PJL INFO NETWORK" + crlf +
		"@PJL INQUIRE IPADDRESS" + crlf +
		"@PJL INQUIRE IPV4" + crlf +
		"@PJL INQUIRE MACADDRESS" + crlf +
		"@PJL DINQUIRE PAGECOUNT" + crlf +
		"@PJL INQUIRE PAGECOUNT" + crlf +
		"@PJL INFO CONFIG" + crlf +
		"@PJL INFO VARIABLES" + crlf +
		uel
}
func (p *HPProfileStandard) ShouldParseSupplies() bool { return true }
func (p *HPProfileStandard) ShouldParseCounters() bool { return true }
func (p *HPProfileStandard) DetermineConfidence(hasSupplies, hasCounters bool) string {
	if hasSupplies || hasCounters {
		return "pjl_full"
	}
	return "pjl_basic"
}

type HPProfileSamsungHeritage struct{}

func (p *HPProfileSamsungHeritage) Name() string { return "HP Samsung-Heritage" }
func (p *HPProfileSamsungHeritage) BuildPJLCommands() string {
	// Las impresoras de herencia Samsung (como la 408dn) tienen un buffer PJL muy pequeño.
	// Si enviamos 500 bytes de comandos de golpe, procesan el primero (INFO ID) y descartan el resto.
	// La solución es envolver CADA comando en UEL (Universal Exit Language) para forzarlas
	// a evaluarlos como "trabajos" separados dentro de la misma transacción USB.

	var sb strings.Builder

	// 1. INFO ID
	sb.WriteString(uel + "@PJL" + crlf + "@PJL INFO ID" + crlf + uel)

	// 2. Comandos Específicos Samsung/HP-Heritage
	// Ya sabemos que ignora el PML crudo. Probaremos los comandos nativos de Samsung
	// para pedir suministros y contadores.
	samsungCmds := []string{
		"@PJL INFO SUPPLIES",
		"@PJL INFO PRODINFO",
		"@PJL INQUIRE TONER",
		"@PJL INQUIRE TONER_CYAN",
		"@PJL INQUIRE TONER_BLACK",
		"@PJL INQUIRE PAGECOUNT",
		"@PJL INQUIRE TOTALPAGECOUNT",
	}

	for _, cmd := range samsungCmds {
		sb.WriteString(uel + "@PJL" + crlf + cmd + crlf + uel)
	}

	// 3. PML Brute Force (por si acaso alguno milagrosamente funciona)
	pmlLines := strings.Split(strings.TrimSpace(GetBruteForcePMLQueries()), "\n")
	for _, line := range pmlLines {
		line = strings.TrimSpace(line)
		if line != "" {
			sb.WriteString(uel + "@PJL" + crlf + line + crlf + uel)
		}
	}

	return sb.String()
}
func (p *HPProfileSamsungHeritage) ShouldParseSupplies() bool { return false }
func (p *HPProfileSamsungHeritage) ShouldParseCounters() bool { return false }
func (p *HPProfileSamsungHeritage) DetermineConfidence(hasSupplies, hasCounters bool) string {
	return "pjl_basic"
}

type GenericProfile struct{}

func (p *GenericProfile) Name() string { return "Generic" }
func (p *GenericProfile) BuildPJLCommands() string {
	return uel + "@PJL" + crlf +
		"@PJL INFO ID" + crlf +
		"@PJL INFO STATUS" + crlf +
		"@PJL INFO SUPPLIES" + crlf +
		"@PJL INFO PRODINFO" + crlf +
		"@PJL INFO NETWORK" + crlf +
		"@PJL INQUIRE IPADDRESS" + crlf +
		"@PJL INQUIRE IPV4" + crlf +
		"@PJL INQUIRE MACADDRESS" + crlf +
		"@PJL DINQUIRE PAGECOUNT" + crlf +
		"@PJL INQUIRE PAGECOUNT" + crlf +
		"@PJL INFO CONFIG" + crlf +
		"@PJL INFO VARIABLES" + crlf +
		uel
}
func (p *GenericProfile) ShouldParseSupplies() bool { return true }
func (p *GenericProfile) ShouldParseCounters() bool { return true }
func (p *GenericProfile) DetermineConfidence(hasSupplies, hasCounters bool) string {
	if hasSupplies || hasCounters {
		return "pjl_full"
	}
	return "pjl_basic"
}

func DetectProfile(model string) BrandProfile {
	upper := strings.ToUpper(model)
	if strings.Contains(upper, "BROTHER") {
		return &BrotherProfile{}
	}
	if strings.Contains(upper, "HP") || strings.Contains(upper, "HEWLETT-PACKARD") || strings.Contains(upper, "HEWLETT PACKARD") {
		// Detect Samsung-heritage models
		if strings.Contains(upper, "408") || strings.Contains(upper, "M40") || strings.Contains(upper, "MFP M") {
			return &HPProfileSamsungHeritage{}
		}
		return &HPProfileStandard{}
	}
	return &GenericProfile{}
}
