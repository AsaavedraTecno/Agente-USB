package payload

import "time"

// Payload es el JSON que se envía al backend Laravel.
type Payload struct {
	SchemaVersion string   `json:"schema_version"`
	EventID       string   `json:"event_id"`
	CollectedAt   string   `json:"collected_at"`
	Source        Source   `json:"source"`
	Printer       Printer  `json:"printer"`
	Counters      Counters `json:"counters"`
	Supplies      []Supply `json:"supplies"`
	Alerts        []Alert  `json:"alerts"`
	Metrics       Metrics  `json:"metrics"`
}

type Source struct {
	AgentID          string `json:"agent_id"`
	// Nota libre del técnico (campo "Nota" de la GUI) — el backend la guarda
	// bajo la clave "label" (DeviceIdentityProcessor::updateAgentStatus()),
	// no "client_name". Va como Label acá para no arrastrar el nombre viejo.
	Label            string `json:"label,omitempty"`
	Hostname         string `json:"hostname"`
	OS               string `json:"os"`
	Version          string `json:"version"`
	Confidence       string `json:"confidence,omitempty"`
	CollectionMethod string `json:"collection_method,omitempty"`
	ConnectedVia     string `json:"connected_via,omitempty"`
}

type Printer struct {
	ID                   string  `json:"id"`
	IP                   *string `json:"ip"`
	Brand                string  `json:"brand"`
	BrandConfidence      float64 `json:"brand_confidence"`
	Model                string  `json:"model"`
	SerialNumber         string  `json:"serial_number"`
	Hostname             string  `json:"hostname"`
	MACAddress           *string `json:"mac_address"`
	Location             *string `json:"location"`
	AssetNumber          *string `json:"asset_number,omitempty"`
	CompanyName          *string `json:"company_name,omitempty"`
	ContactPerson        *string `json:"contact_person,omitempty"`
	Firmware             *string `json:"firmware,omitempty"`
	Status               string  `json:"status,omitempty"`
	Display              *string `json:"display,omitempty"`
	DuplexCountingMethod *string `json:"duplex_counting_method,omitempty"`
	Trays                []Tray  `json:"trays"`
}

type Tray struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	PaperSize  string `json:"paper_size"`
	Capacity   *int   `json:"capacity"`
	Level      *int   `json:"level"`
	Percentage *int   `json:"percentage"`
}

type Counters struct {
	Absolute      CountersAbsolute      `json:"absolute"`
	LogicalMatrix CountersLogicalMatrix `json:"logical_matrix"`
	HardwareUsage CountersHardwareUsage `json:"hardware_usage"`
	Delta         *CountersDelta        `json:"delta"`
	ResetDetected *bool                 `json:"reset_detected,omitempty"`
	Confidence    string                `json:"confidence"`
	CoverageLast  *float64              `json:"coverage_last"`
	CoverageAvg   *float64              `json:"coverage_avg"`
}

type CountersDelta struct {
	TotalPages        *int64 `json:"total_pages"`
	MonoPages         *int64 `json:"mono_pages"`
	ColorPages        *int64 `json:"color_pages"`
	ScanPages         *int64 `json:"scan_pages"`
	CopyPages         *int64 `json:"copy_pages"`
	FaxPages          *int64 `json:"fax_pages"`
	SimplexPages      *int64 `json:"simplex_pages"`
	EngineCycles      *int64 `json:"engine_cycles"`
	EngineCyclesMono  *int64 `json:"engine_cycles_mono"`
	EngineCyclesColor *int64 `json:"engine_cycles_color"`
}

type CountersAbsolute struct {
	Total           *int64 `json:"total"`
	Mono            *int64 `json:"mono"`
	Color           *int64 `json:"color"`
	EquivalentTotal *int64 `json:"equivalent_total,omitempty"`
	EquivalentMono  *int64 `json:"equivalent_mono,omitempty"`
	EquivalentColor *int64 `json:"equivalent_color,omitempty"`
}

type CountersLogicalMatrix struct {
	ByFunction    CountersByFunction    `json:"by_function"`
	ByMode        CountersByMode        `json:"by_mode"`
	ByDestination CountersByDestination `json:"by_destination"`
}

type CountersByFunction struct {
	Print      *int64 `json:"print"`
	PrintMono  *int64 `json:"print_mono,omitempty"`
	PrintColor *int64 `json:"print_color,omitempty"`
	Copy       *int64 `json:"copy"`
	CopyMono   *int64 `json:"copy_mono,omitempty"`
	CopyColor  *int64 `json:"copy_color,omitempty"`
	FaxPrint   *int64 `json:"fax_print"`
	Reports    *int64 `json:"reports"`
}

type CountersByMode struct {
	Simplex           *int64 `json:"simplex"`
	Duplex            *int64 `json:"duplex,omitempty"`
	DuplexSheets      *int64 `json:"duplex_sheets,omitempty"`
	DuplexMono        *int64 `json:"duplex_mono,omitempty"`
	DuplexColor       *int64 `json:"duplex_color,omitempty"`
	DuplexImpressions *int64 `json:"duplex_impressions,omitempty"`
}

type CountersByDestination struct {
	Email     *int64 `json:"email"`
	Ftp       *int64 `json:"ftp"`
	Smb       *int64 `json:"smb"`
	Usb       *int64 `json:"usb"`
	Others    *int64 `json:"others"`
	TotalSend *int64 `json:"total_send"`
}

type CountersHardwareUsage struct {
	TotalScans        *int64 `json:"total_scans"`
	EngineCycles      *int64 `json:"engine_cycles"`
	EngineCyclesMono  *int64 `json:"engine_cycles_mono,omitempty"`
	EngineCyclesColor *int64 `json:"engine_cycles_color,omitempty"`
	JamTotal          *int64 `json:"jam_total,omitempty"`
	JamTray1          *int64 `json:"jam_tray1,omitempty"`
	JamTray2          *int64 `json:"jam_tray2,omitempty"`
	JamTrayMP         *int64 `json:"jam_tray_mp,omitempty"`
	JamInside         *int64 `json:"jam_inside,omitempty"`
	JamRear           *int64 `json:"jam_rear,omitempty"`
}

type Supply struct {
	ID              string   `json:"id"`
	Type            string   `json:"type"`
	SubType         string   `json:"sub_type,omitempty"`
	Color           string   `json:"color"`
	Category        string   `json:"category,omitempty"`
	IsWaste         *bool    `json:"is_waste,omitempty"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	SerialNumber    *string  `json:"serial_number,omitempty"`
	Model           *string  `json:"model,omitempty"`
	IsOriginal      *bool    `json:"is_original,omitempty"`
	Percentage      *float64 `json:"percentage"`
	Status          string   `json:"status"`
	IsMeasurable    bool     `json:"is_measurable"`
	SnmpTypeID      *int     `json:"snmp_type_id,omitempty"`
	RawLevel        *int     `json:"raw_level"`
	RawMax          *int     `json:"raw_max"`
	PagesWithSupply *int     `json:"pages_with_supply,omitempty"`
	InstallDate     *string  `json:"install_date,omitempty"`
	CartridgeType   *string  `json:"cartridge_type,omitempty"`
	ChangeCount     *int     `json:"change_count,omitempty"`
	Genuine         *bool    `json:"genuine,omitempty"`
}

type Alert struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Severity   string `json:"severity"`
	Message    string `json:"message"`
	DetectedAt string `json:"detected_at"`
}

type Metrics struct {
	UptimeSeconds *int64         `json:"uptime_seconds"`
	PowerOnCount  *int64         `json:"power_on_count"`
	Polling       MetricsPolling `json:"polling"`
}

type MetricsPolling struct {
	ResponseTimeMs int64   `json:"response_time_ms"`
	PollDurationMs int64   `json:"poll_duration_ms"`
	OidSuccessRate float64 `json:"oid_success_rate"`
	RetryCount     int     `json:"retry_count"`
	LastPollAt     string  `json:"last_poll_at"`
	NextPollAt     *string `json:"next_poll_at"`
	ErrorCount     int     `json:"error_count"`
}

func New(agentID, hostname, version string) *Payload {
	now := time.Now().UTC().Format(time.RFC3339)
	return &Payload{
		SchemaVersion: "1.0.0",
		EventID:       "", // To be filled later
		CollectedAt:   now,
		Source: Source{
			AgentID:          agentID,
			Hostname:         hostname,
			OS:               "windows",
			Version:          version,
			CollectionMethod: "usb_direct",
			ConnectedVia:     "usb",
		},
		Printer:  Printer{Trays: []Tray{}},
		Counters: Counters{},
		Supplies: []Supply{},
		Alerts:   []Alert{},
		Metrics: Metrics{
			Polling: MetricsPolling{LastPollAt: now},
		},
	}
}

func Int64Ptr(v int64) *int64       { return &v }
func StrPtr(v string) *string       { return &v }
func Float64Ptr(v float64) *float64 { return &v }
func IntPtr(v int) *int             { return &v }
func BoolPtr(v bool) *bool          { return &v }
