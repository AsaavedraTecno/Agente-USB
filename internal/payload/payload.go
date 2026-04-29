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
	DeviceAlerts  []string `json:"device_alerts"`
	Metrics       Metrics  `json:"metrics"`
}

type Source struct {
	AgentID          string `json:"agent_id"`
	Hostname         string `json:"hostname"`
	OS               string `json:"os"`
	Version          string `json:"version"`
	Confidence       string `json:"confidence,omitempty"`
	CollectionMethod string `json:"collection_method,omitempty"`
	ConnectedVia     string `json:"connected_via,omitempty"`
}

type Printer struct {
	ID              string  `json:"id"`
	IP              *string `json:"ip"`
	Brand           string  `json:"brand"`
	BrandConfidence float64 `json:"brand_confidence"`
	Model           string  `json:"model"`
	SerialNumber    string  `json:"serial_number"`
	Hostname        string  `json:"hostname"`
	MACAddress      *string `json:"mac_address"`
	Location        *string `json:"location"`
	Firmware        *string `json:"firmware,omitempty"`
	Status          string  `json:"status,omitempty"`
	Trays           []Tray  `json:"trays"`
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
	Confidence    string                `json:"confidence"`
	CoverageLast  *float64              `json:"coverage_last"`
	CoverageAvg   *float64              `json:"coverage_avg"`
}

type CountersAbsolute struct {
	Total *int64 `json:"total"`
	Mono  *int64 `json:"mono"`
	Color *int64 `json:"color"`
}

type CountersLogicalMatrix struct {
	ByFunction    CountersByFunction    `json:"by_function"`
	ByMode        CountersByMode        `json:"by_mode"`
	ByDestination CountersByDestination `json:"by_destination"`
}

type CountersByFunction struct {
	Print    *int64 `json:"print"`
	Copy     *int64 `json:"copy"`
	FaxPrint *int64 `json:"fax_print"`
	Reports  *int64 `json:"reports"`
}

type CountersByMode struct {
	Simplex *int64 `json:"simplex"`
	Duplex  *int64 `json:"duplex"`
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
	TotalScans   *int64 `json:"total_scans"`
	EngineCycles *int64 `json:"engine_cycles"`
	JamTotal     *int64 `json:"jam_total"`
	JamTray1     *int64 `json:"jam_tray1"`
	JamTray2     *int64 `json:"jam_tray2"`
	JamTrayMP    *int64 `json:"jam_tray_mp"`
	JamInside    *int64 `json:"jam_inside"`
	JamRear      *int64 `json:"jam_rear"`
}

type Supply struct {
	ID            string   `json:"id"`
	Type          string   `json:"type"`
	Color         string   `json:"color"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Percentage    *float64 `json:"percentage"`
	Status        string   `json:"status"`
	IsMeasurable  bool     `json:"is_measurable"`
	RawLevel      *int     `json:"raw_level"`
	RawMax        *int     `json:"raw_max"`
	SerialNumber  *string  `json:"serial_number,omitempty"`
	CartridgeType *string  `json:"cartridge_type,omitempty"`
	ChangeCount   *int     `json:"change_count,omitempty"`
	Genuine       *bool    `json:"genuine,omitempty"`
}

type Alert struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Level   string `json:"level"`
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
	NextPollAt     string  `json:"next_poll_at"`
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
		Printer:      Printer{Trays: []Tray{}},
		Counters:     Counters{},
		Supplies:     []Supply{},
		DeviceAlerts: []string{},
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
