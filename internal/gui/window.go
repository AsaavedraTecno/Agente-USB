package gui

import (
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"usb-agent/internal/agent"
	"usb-agent/internal/config"
	"usb-agent/internal/uploader"
	"usb-agent/internal/winsvc"
)

// forceDarkTheme fuerza un tema de alto contraste (mismo look que AgenteSNMP),
// independiente del tema claro/oscuro del sistema operativo.
type forceDarkTheme struct{}

func (m *forceDarkTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNameBackground:
		return color.NRGBA{R: 15, G: 15, B: 15, A: 255} // Fondo casi negro
	case theme.ColorNameForeground:
		return color.NRGBA{R: 255, G: 255, B: 255, A: 255} // Texto normal blanco
	case theme.ColorNameDisabled:
		return color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	case theme.ColorNameInputBackground:
		return color.NRGBA{R: 40, G: 40, B: 40, A: 255} // Caja de texto gris oscuro
	case theme.ColorNamePlaceHolder:
		return color.NRGBA{R: 180, G: 180, B: 180, A: 255}
	}
	return theme.DefaultTheme().Color(name, theme.VariantDark)
}

func (m *forceDarkTheme) Font(style fyne.TextStyle) fyne.Resource {
	return theme.DefaultTheme().Font(style)
}

func (m *forceDarkTheme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return theme.DefaultTheme().Icon(name)
}

func (m *forceDarkTheme) Size(name fyne.ThemeSizeName) float32 {
	return theme.DefaultTheme().Size(name)
}

// maskKey oculta la key salvo los últimos 5 caracteres, para que coincida con
// lo que muestra el panel web (mismo criterio que AgenteSNMP).
func maskKey(key string) string {
	clean := strings.TrimSpace(key)
	if len(clean) <= 5 {
		return "**********"
	}
	return "**********" + clean[len(clean)-5:]
}

// RunWindow abre la ventana principal del Agente USB Monitor.
// startHidden abre solo el ícono de bandeja (usado por el acceso directo de
// inicio automático, "--tray"), sin interrumpir el inicio de sesión con una
// ventana.
func RunWindow(startHidden bool) {
	a := app.New()
	a.Settings().SetTheme(&forceDarkTheme{})

	w := a.NewWindow("TecnoData Agente USB  v" + agent.Version)

	cfg := config.LoadPortable()
	agentID := config.GetOrCreateAgentID(cfg)

	// ── Barra de estado superior ──────────────────────────────────────────

	statusLabel := widget.NewLabel("ESTADO: CARGANDO...")
	statusLabel.TextStyle = fyne.TextStyle{Bold: true}

	// ── Agent Key ─────────────────────────────────────────────────────────

	isConfigured := cfg.APIKey != ""

	keyEntry := widget.NewEntry()
	if isConfigured {
		keyEntry.Text = "🔒 " + maskKey(cfg.APIKey)
		keyEntry.Disable()
	} else {
		keyEntry.SetPlaceHolder("Ingresa tu Agent Key aquí...")
	}

	activateStatus := widget.NewLabel("")

	// "Probar Conexión": verificación inmediata para el técnico al instalar, sin
	// depender de ningún ciclo del servicio (que puede tardar hasta interval_minutes
	// en correr). Usa la key recién tipeada si todavía no se guardó, o la ya
	// guardada si el campo está bloqueado/enmascarado.
	connLabel := widget.NewLabel("○  Sin verificar")
	testBtn := widget.NewButtonWithIcon("Probar Conexión", theme.MediaFastForwardIcon(), func() {
		connLabel.SetText("…  Probando...")
		key := strings.ToUpper(strings.TrimSpace(keyEntry.Text))
		if keyEntry.Disabled() {
			key = config.LoadPortable().APIKey
		}
		go func() {
			c := config.LoadPortable()
			winsvc.LogEvent("gui", "Probar Conexión — server_url=%q", c.ServerURL)
			_, err := uploader.FetchConfig(c.ServerURL, key, agentID, c.SkipTLSVerify)
			fyne.Do(func() {
				if err != nil {
					connLabel.SetText("✗  Sin respuesta")
					winsvc.LogEvent("gui", "Probar Conexión falló: %v", err)
				} else {
					connLabel.SetText("✓  Conectado y autorizado")
					winsvc.LogEvent("gui", "Probar Conexión OK")
				}
			})
		}()
	})

	// "Extraer Datos Ahora": corre un ciclo real (sondear la impresora + mandar
	// telemetría) en el momento, sin esperar el próximo tick programado — el
	// técnico en terreno quiere saber ya si el USB escanea bien y si el envío
	// a la nube funciona, no recién dentro de interval_minutes. El sondeo
	// periódico (cada interval_minutes/scan_interval) sigue corriendo solo,
	// esto es un disparo manual extra sobre el mismo ciclo (winsvc.RunCycle).
	extractLabel := widget.NewLabel("")
	extractBtn := widget.NewButtonWithIcon("Extraer Datos Ahora", theme.DownloadIcon(), nil)
	extractBtn.OnTapped = func() {
		extractBtn.Disable()
		extractLabel.SetText("…  Extrayendo y enviando...")
		go func() {
			c := config.LoadPortable()
			err := winsvc.RunCycle(c)
			fyne.Do(func() {
				if err != nil {
					extractLabel.SetText("✗  Falló — ver pestaña Logs")
				} else {
					extractLabel.SetText("✓  Listo — ver pestaña Logs")
				}
				extractBtn.Enable()
			})
		}()
	}

	var saveBtn *widget.Button
	saveBtn = widget.NewButtonWithIcon("Activar Agente", theme.ConfirmIcon(), func() {
		cleanKey := strings.ToUpper(strings.TrimSpace(keyEntry.Text))
		if cleanKey == "" {
			return
		}
		c := config.LoadPortable()
		c.APIKey = cleanKey
		c.AgentID = agentID
		if err := c.SavePortable(); err != nil {
			dialog.ShowError(err, w)
			return
		}
		keyEntry.Text = "🔒 " + maskKey(cleanKey)
		keyEntry.Disable()
		saveBtn.Hide()
		activateStatus.SetText("✓ Activado")
	})
	if isConfigured {
		saveBtn.Hide()
	}

	// ── Nota (opcional) ───────────────────────────────────────────────────
	// A diferencia de la Agent Key, queda siempre editable — no se bloquea
	// al guardar. Se envía en cada reporte como identificación del cliente/
	// ubicación de esta instalación.

	labelEntry := widget.NewMultiLineEntry()
	labelEntry.Wrapping = fyne.TextWrapWord
	labelEntry.SetMinRowsVisible(3)
	labelEntry.SetPlaceHolder("Ej: Farmacia Central, Clínica Norte, Oficina Stgo")
	labelEntry.SetText(cfg.ClientName)

	labelStatus := widget.NewLabel("")
	labelSaveBtn := widget.NewButtonWithIcon("Guardar nota", theme.ConfirmIcon(), func() {
		c := config.LoadPortable()
		c.ClientName = strings.TrimSpace(labelEntry.Text)
		c.AgentID = agentID
		_ = c.SavePortable()
		labelStatus.SetText("✓ Guardado — se enviará en el próximo reporte")
	})

	hostname, _ := os.Hostname()

	configView := container.NewPadded(container.NewVBox(
		widget.NewLabelWithStyle("CONFIGURACIÓN", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		widget.NewSeparator(),
		widget.NewLabel("Agent Key:"),
		keyEntry,
		container.NewHBox(saveBtn, testBtn, connLabel),
		activateStatus,
		container.NewHBox(extractBtn, extractLabel),
		widget.NewSeparator(),
		widget.NewLabel("Nota (opcional):"),
		labelEntry,
		labelSaveBtn,
		labelStatus,
		layout.NewSpacer(),
		widget.NewLabelWithStyle("Host: "+hostname, fyne.TextAlignCenter, fyne.TextStyle{Italic: true}),
	))

	// ── Vista de logs ─────────────────────────────────────────────────────

	logArea := widget.NewMultiLineEntry()
	logArea.Disable()
	logArea.Wrapping = fyne.TextWrapBreak
	logArea.TextStyle = fyne.TextStyle{Monospace: true}

	logsView := container.NewBorder(
		widget.NewLabelWithStyle("REGISTROS DE ACTIVIDAD", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		nil, nil, nil,
		logArea,
	)

	// ── Navegación lateral ────────────────────────────────────────────────

	contentArea := container.NewStack(configView)

	sideMenu := widget.NewList(
		func() int { return 2 },
		func() fyne.CanvasObject {
			return container.NewHBox(widget.NewIcon(theme.InfoIcon()), widget.NewLabel("Item"))
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			box := obj.(*fyne.Container)
			icon := box.Objects[0].(*widget.Icon)
			label := box.Objects[1].(*widget.Label)
			if id == 0 {
				label.SetText("Estado")
				icon.SetResource(theme.SettingsIcon())
			} else {
				label.SetText("Logs")
				icon.SetResource(theme.DocumentIcon())
			}
		},
	)

	sideMenu.OnSelected = func(id widget.ListItemID) {
		if id == 0 {
			contentArea.Objects = []fyne.CanvasObject{configView}
		} else {
			contentArea.Objects = []fyne.CanvasObject{logsView}
		}
		contentArea.Refresh()
	}

	// ── Monitoreo en segundo plano: estado del servicio + tail del log ─────
	// La instalación/desinstalación del servicio ya no se hace desde la GUI —
	// la maneja el instalador (setup_script.iss) al momento de instalarse.

	logPath := filepath.Join(config.ExeDir(), "agent.log")

	go func() {
		for {
			st := winsvc.Status()
			var text string
			switch st {
			case "ejecutando":
				text = "🟢 SERVICIO ACTIVO"
			case "detenido":
				text = "🔴 SERVICIO DETENIDO"
			case "no instalado":
				text = "⚠️ SERVICIO NO INSTALADO"
			case "iniciando...", "deteniendo...":
				text = "⏳ " + strings.ToUpper(st)
			default:
				text = "❔ NO SE PUDO LEER EL ESTADO"
			}
			fyne.Do(func() { statusLabel.SetText(text) })

			if content, err := os.ReadFile(logPath); err == nil {
				str := string(content)
				if len(str) > 4000 {
					str = str[len(str)-4000:]
				}
				fyne.Do(func() {
					logArea.SetText(str)
					logArea.CursorColumn = len(str)
				})
			}
			time.Sleep(3 * time.Second)
		}
	}()

	// ── Layout final ──────────────────────────────────────────────────────

	mainSplit := container.NewHSplit(sideMenu, contentArea)
	mainSplit.Offset = 0.25

	finalLayout := container.NewBorder(
		container.NewPadded(statusLabel),
		nil, nil, nil,
		mainSplit,
	)

	w.SetContent(finalLayout)
	w.Resize(fyne.NewSize(650, 550))
	w.SetMaster()

	w.SetCloseIntercept(func() { w.Hide() })

	if desk, ok := a.(desktop.App); ok {
		m := fyne.NewMenu("Agente USB",
			fyne.NewMenuItem("Abrir", func() { w.Show() }),
			fyne.NewMenuItem("Salir", func() { a.Quit() }),
		)
		desk.SetSystemTrayMenu(m)
	}

	if startHidden {
		// No w.Show(): el ícono de bandeja (ya configurado arriba) alcanza —
		// el usuario la abre desde ahí con "Abrir" si necesita revisar algo.
		a.Run()
	} else {
		w.ShowAndRun()
	}
}
