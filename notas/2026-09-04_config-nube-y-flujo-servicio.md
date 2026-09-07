# Config real desde la nube en el servicio: diseño del loop de control

**Fecha:** 2026-09-04
**Estado:** Diseño aprobado, pendiente de implementar (ver plan de la sesión).

## El problema de fondo

`uploader.FetchConfig()` ya pega a `GET /api/agent/config` en cada handshake (`runner.go:49`), pero descarta la respuesta completa — hoy es un ping de conectividad, no config real. El backend ya devuelve, para cualquier tipo de agente sin distinguir quién pregunta:

```json
{ "active": true, "scan_interval": 900, "snmp_community": "...", "snmp_version": "...", "discovery": {...}, "max_concurrent": 50 }
```

`active` (kill-switch) y `scan_interval` (segundos) son genéricos y sirven tal cual para USB. El resto no aplica y se ignora sin problema (Go descarta campos JSON no mapeados a un struct).

**Backend hoy: no distingue USB de SNMP en este endpoint.** Mismo endpoint, misma respuesta genérica, sin ningún `agent_type` ni rama de código que mire quién llama. No hace falta tocar el backend para esto — no se tocó nada de `tdmonitor` en este diseño.

## Por qué NO copiar el patrón de AgenteSNMP tal cual (60-75s con jitter)

AgenteSNMP separa dos loops — control (pregunta `/agent/config` cada 60s + jitter random 0-15s, `IntervaloControl` en `cmd/agent_tdmonitor/main.go:76,886`) y datos (el escaneo SNMP en sí) — con una razón explícita en su propio comentario: *"obedecer órdenes quiere ser rápido y barato, escanear quiere ser espaciado y caro; cuando compartían un solo bucle, la latencia de CUALQUIER orden era el scan_interval (hasta 1 hora)"*.

El motivo real para copiarlo sería el mismo: que el kill-switch no tarde tanto como el intervalo de datos. Pero el número (60s) no es arbitrario para SNMP — está calibrado para su topología (1-3 agentes por cliente, cubriendo la WAN completa). Copiarlo ciego a USB es el peor ajuste posible:

- `/agent/config` tiene `throttle:120,1` = **120 req/min por IP** (`routes/api.php:35`, verificado en código, no en notas).
- A 60s, ~120 agentes detrás de la misma IP ya rompen el límite. Con 500 agentes en un solo edificio compartiendo salida a internet (escenario plausible para USB, no para SNMP), son ~440 req/min agregados — 4x sobre el límite.
- SNMP es 1-3 agentes por cliente (una WAN completa). USB es el caso opuesto: es normal tener muchas notebooks con impresora USB en el mismo edificio. El escenario que rompe el límite es mucho más realista para USB que para SNMP.

## Decisión: separar las dos necesidades reales, sin loop de fondo agresivo

1. **Técnico instalando, verificación inmediata** → botón "Probar Conexión" en la GUI (manual, sin ningún loop). Se recupera solo para esto — el resto de lo que se sacó en la simplificación de esta sesión (ID de agente visible, sección de servicio, "Ejecutar Ahora") sigue afuera.
2. **Operación normal** → un chequeo de config cada 30 minutos (no cada 60s), separado de cuándo realmente se sondea la impresora.

### Por qué 30 min

La capacidad por IP escala linealmente con el intervalo (= 120 × minutos del intervalo). A 30 min hacen falta ~3,600 agentes en la misma IP para romper el límite — margen amplio para crecimiento real, muy por encima de los ~120 que rompía el patrón de 60s.

## El diseño: un solo ticker, sin goroutine ni mutex extra

Se evaluó (y se descartó) una segunda goroutine con su propio `time.Ticker` de 30 min, compartiendo estado con el loop de datos vía `sync.RWMutex`. Funciona, pero agrega concurrencia que no hace falta.

**Mejor opción, misma responsividad:** un solo `time.Ticker` al intervalo fino (30 min), con un contador de ticks que decide cada cuánto toca hacer el trabajo pesado (sondear la impresora). Todo pasa secuencialmente dentro del `select` que ya existe hoy en `winsvc/service.go::Execute()` — nada corre en paralelo que necesite protegerse.

```go
controlInterval := 30 * time.Minute
if dataInterval < controlInterval {
    controlInterval = dataInterval // ej. interval_minutes=15 -> no tiene sentido chequear cada 30
}
ticksPerCycle := max(1, dataInterval / controlInterval)
tick := 0

ticker := time.NewTicker(controlInterval)
for {
    select {
    case <-ticker.C:
        rc, err := uploader.FetchConfig(cfg.ServerURL, cfg.APIKey, cfg.SkipTLSVerify) // liviano, cada tick
        if err == nil && rc != nil && rc.ScanInterval > 0 {
            dataInterval = time.Duration(rc.ScanInterval) * time.Second
            ticksPerCycle = max(1, dataInterval/controlInterval) // recalcula, el ticker NO se resetea
        }
        tick++
        if tick >= ticksPerCycle {
            tick = 0
            if err == nil && rc != nil && !rc.Active {
                lg("agente pausado desde la nube")
            } else {
                cfg = config.LoadPortable()
                go runCycle(cfg) // el trabajo pesado (USB) solo dispara acá
            }
        }
    case c := <-r:
        // igual que hoy: Stop/Shutdown
    }
}
```

### Ejemplo concreto con `interval_minutes = 120` (2 horas)

`controlInterval=30min`, `ticksPerCycle=4`:

```
t=0min   → arranca el servicio → EXTRAE ahora mismo (igual que hoy, sin cambios)
t=30min  → tick 1: pregunta /agent/config (liviano, no toca la impresora) → NO extrae (1<4)
t=60min  → tick 2: pregunta /agent/config → NO extrae (2<4)
t=90min  → tick 3: pregunta /agent/config → NO extrae (3<4)
t=120min → tick 4: pregunta /agent/config → SÍ extrae de la impresora (4>=4, contador vuelve a 0)
t=150min → tick 1 → ... se repite cada 2h para la extracción real ...
```

La extracción de la impresora **sigue dependiendo 100% de `interval_minutes`/`scan_interval`**, sin cambio de cadencia — el chequeo de 30 min nunca toca la impresora ni genera tráfico USB, solo pregunta al servidor. Si `interval_minutes` fuera menor a 30 (ej. 15), `controlInterval` se ajusta a ese valor directo y `ticksPerCycle=1` — vuelve al comportamiento simple de "revisar y extraer en el mismo tick".

## ⚠️ Hallazgo posterior a implementar: `active` NO es un kill-switch genérico (revisar antes de confiar en él)

**Fecha:** 2026-09-04, mismo día, después de implementar el diseño de arriba.

Al probar "Probar Conexión" contra una key real recién creada en `qatdmonitor.cl`, se revisó `AgentController::getAgentConfig()` (código real, líneas ~468-654) y se encontró que `active` **no es un interruptor manual genérico** como asumía este diseño — es un derivado del estado de descubrimiento SNMP:

- **Primer contacto de una key nueva** → entra en modo Descubrimiento (`descubrimiento_pendiente` o ventana de tiempo vencida) → `active: true`, pero es temporal: se apaga solo (`descubrimiento_pendiente = false`) al servir esa respuesta.
- **Después de esa ventana** → modo Recolección (el 99% del tiempo): `discoveryEnabled = $printers->isNotEmpty()`, es decir, `active` pasa a depender de si hay impresoras con `is_networked=true` e `ip_address` aprobadas para esa key.

Una impresora **USB normalmente no tiene IP de red** (no es `is_networked`), así que para una key usada solo por AgenteUSB, `$printers` va a estar vacío casi siempre → **`active` se apaga solo después de la primera ventana de descubrimiento, para siempre**, sin que nadie lo pause a propósito.

**Impacto concreto:** el loop de control implementado en `winsvc/service.go::Execute()` (arriba) SÍ obedece este `active` — así que, tal como quedó, un AgenteUSB en producción se pausaría solo después de un rato, de forma silenciosa (queda log de "agente pausado desde la nube", pero la causa real no es una pausa intencional sino un falso negativo del backend).

**Resuelto 2026-09-06, opción 2** (repo `tdmonitor`, `AgentController::buildRemoteConfigPayload()`):

```php
$estaActivo = $agentKey->agent_type === 'usb'
    ? true
    : (bool) ($rawConfig['discovery']['enabled'] ?? false);
```

Una Llave `usb` ahora siempre manda `active: true` — el kill-switch remoto vía
`active` queda sin efecto para USB (se puede revocar la Llave entera, que sí
corta el acceso, pero no "pausar sin revocar"). SNMP no cambia: sigue
dependiendo de `discovery.enabled` exactamente como antes. Test:
`tests/Feature/AgentController/ActiveFlagUsbTest.php` (repo `tdmonitor`) —
reproduce el estado post-primera-ventana (sin impresoras is_networked) y
verifica que USB queda activo y SNMP sigue igual que siempre.

Sin este fix, un AgenteUSB en producción mandaba telemetría una vez al
instalar y quedaba "pausado desde la nube" el resto del tiempo, hasta el
próximo barrido automático cada `HORAS_ENTRE_DESCUBRIMIENTOS` (72h) —
encontrado al auditar la arquitectura completa, no reportado por un cliente.

## Brechas identificadas en el camino, pendientes de decidir

- **`skip_tls_verify: true` es el default hoy** (`internal/config/config.go::defaults()`). Mientras la respuesta de `/agent/config` se ignoraba, daba igual que alguien la falsificara (MITM). Ahora que el agente actúa sobre ella (pausar el ciclo, cambiar el intervalo), sí importa — peor caso realista: DoS de monitoreo (pausar agentes a voluntad simulando `active:false`), no fuga de datos. Arreglo: cambiar el default a `false` — gratis si el backend tiene certificado válido (debería, es producción), pero rompería la conexión si no lo tiene. Confirmar antes de cambiar.
- **Backoff en 429** — si algún día la realidad supera el margen calculado (30 min → ~3,600 agentes/IP), que el agente detecte un 429 y espere progresivamente en vez de seguir insistiendo cada 30 min. Barato de agregar, no bloqueante para la primera versión.
- **`internal/queue/queue.go` sin purga** de archivos viejos (AgenteSNMP sí purga a las 72h, `pkg/uploader/uploader.go::purgeOldFiles`) — gap aparte, no forma parte de este diseño.
- **`agent.log` sin rotación** (`winsvc/service.go::runCycle()`, `O_APPEND` para siempre) — mismo tipo de gap, AgenteSNMP lo resuelve con `lumberjack`.

## Ver también

- Plan de la sesión: `ahora-quiero-convertir-este-purrfect-puppy.md` (histórico de la sesión completa que llevó a este diseño).
- `NOTES.md` (raíz del repo) — el gap de `pages_with_supply`/`change_count` forzados a `nil` después de la primera lectura, pendiente de revisar aparte cuando se rearquitecture el tracking de suministros.
- `Notas/TDMonitor_Arquitectura_Agentes_USB.md` (repo `tdmonitor`) — contexto más amplio de por qué USB y SNMP quedan como agentes Go independientes sin librería compartida.
