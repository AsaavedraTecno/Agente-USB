# Notas pendientes

## `pages_with_supply` / `change_count` se borran después de la primera lectura

**Dónde:** [internal/state/state.go](internal/state/state.go) función `CalculateDelta`, líneas ~166-174.

```go
for i, s := range curr.Supplies {
    if prevSup, ok := prev.Supplies[s.ID]; ok {
        // El usuario solicitó remover el algoritmo de change_count y pages_with_supply
        // para evitar falsos positivos por fluctuaciones menores de los sensores.
        curr.Supplies[i].PagesWithSupply = nil
        curr.Supplies[i].ChangeCount = nil
        _ = prevSup
    }
}
```

**Comportamiento actual:** la primera vez que se lee una impresora (no hay estado previo guardado
en `state/printer_<ID>.json`), el payload trae `pages_with_supply` y `change_count` normalmente.
En **todas las corridas siguientes** para esa misma impresora, estos dos campos se fuerzan a `nil`
a propósito — fue un pedido explícito anterior para evitar falsas alarmas de "cambio de cartucho"
por ruido de sensores.

**Por qué importa ahora:** al agregar el respaldo EWS-sobre-USB para la HP LaserJet E40040
([internal/hpprotocol/ews_usb.go](internal/hpprotocol/ews_usb.go)), se puede extraer
`pages_with_supply` (páginas impresas con el tóner actual) directamente y de forma confiable desde
la página "Supplies Status" del EWS — no es un valor de sensor ruidoso, es un contador que la propia
impresora reporta. Con la lógica actual, ese dato solo se ve en la primera lectura y después se
pierde en cada poll periódico (cada `interval_minutes` en `agent.yaml`).

**Pendiente:** al rearquitecturar esto, decidir si `pages_with_supply`/`change_count` deben:
- seguir borrándose siempre después de la primera lectura (comportamiento actual), o
- reportarse en cada corrida cuando el origen del dato es confiable (como el EWS, que da un
  contador exacto en vez de un nivel de sensor), reservando el borrado solo para fuentes que sí
  son ruidosas (ej. protocolo binario Samsung/PJL).

No se tocó nada de esto — se dejó el comportamiento tal cual estaba a pedido del usuario
(2026-08-26) hasta que se revise la arquitectura completa de tracking de suministros.
