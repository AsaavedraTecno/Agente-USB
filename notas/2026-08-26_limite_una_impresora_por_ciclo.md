# Solo se reporta 1 impresora por ciclo, aunque haya varias conectadas

**Fecha:** 2026-08-26
**Estado:** Pendiente. Encontrado leyendo el código para explicar la arquitectura, no reportado por un síntoma.

## Dónde

[internal/agent/runner.go:71-74](internal/agent/runner.go#L71):

```go
for i, pr := range printers {
    lf("  [%d] %-40s  Puerto: %s", i+1, pr.Name, pr.PortName)
}
target := printers[0]
```

`discovery.FindUSBPrinters()` (internal/discovery/discovery_windows.go) devuelve **todas** las
impresoras USB que Windows tiene instaladas en ese PC. El `for` de arriba solo las **lista en el
log** — el resto de `Run()` (extracción, delta, payload, cola, envío) trabaja únicamente sobre
`printers[0]`. No hay ningún otro lugar en `runner.go` que itere sobre `printers` para construir
más de un payload por ciclo — confirmado con `grep -n "range printers"` en todo el archivo,
un solo resultado.

## Por qué importa

`FindUSBPrinters()` arma la lista con `Get-WmiObject Win32_Printer` (PowerShell). WMI **no
garantiza un orden estable** entre llamadas — no hay `Sort-Object` ni nada que lo fuerce en
`discovery_windows.go`. Consecuencia en un PC con 2+ impresoras USB:

- Solo una queda cubierta por ciclo — la otra no se reporta ese ciclo, ningún error visible.
- No es necesariamente siempre la misma: el orden puede cambiar entre reinicios del servicio,
  reconexión de una impresora, actualización de driver, etc. — cobertura inconsistente, no un
  fallo limpio y predecible.
- Con un solo dispositivo USB por PC (el caso típico que se está desplegando ahora) esto no se
  nota. Se vuelve un problema real el día que alguien conecte una segunda impresora al mismo PC
  y asuma que también se está monitoreando.

## Pendiente (no implementado, solo diagnóstico)

Cambiar el `target := printers[0]` por un loop real que arme y encole un payload por cada
impresora detectada — mismo pipeline de extracción/delta/cola que ya existe hoy, corrido N
veces en vez de una. `EventID` ya incluye `p.Printer.ID` (serie o MAC), así que dos payloads del
mismo ciclo para impresoras distintas no colisionarían del lado del servidor.
