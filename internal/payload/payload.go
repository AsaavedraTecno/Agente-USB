package payload

import "time"

// Payload es el JSON que se envía al backend Laravel.
// Estructura idéntica al agente de red existente, con dos campos nuevos en Source.
type Payload struct {
	Source       Source   `json:"source"`
	Printer      Printer  `json:"printer"`
	Counters     Counters `json:"counters"`
	Supplies     []Supply `json:"supplies"`
	DeviceAlerts []Alert  `json:"device_alerts"`
	Metrics      Metrics  `json:"metrics"`
}

type Source struct {
	AgentID          string `json:"agent_id"`
	Hostname         string `json:"hostname"`
	OS               string `json:"os"`
	Version          string `json:"version"`
	Confidence       string `json:"confidence"`
	CollectionMethod string `json:"collection_method"` // usb_direct | network_snmp
	ConnectedVia     string `json:"connected_via"`     // usb | network
	Timestamp        string `json:"timestamp"`
}

type Printer struct {
	IP     *string `json:"ip"`
	Brand  string  `json:"brand"`
	Model  string  `json:"model"`
	Serial string  `json:"serial"`
	MAC    *string `json:"mac"`
	Trays  []Tray  `json:"trays"`
}

type Tray struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	MediaType string `json:"media_type"`
}

type Counters struct {
	TotalPages   *int64 `json:"total_pages"`
	PrintPages   *int64 `json:"print_pages"`
	CopyPages    *int64 `json:"copy_pages"`
	FaxPages     *int64 `json:"fax_pages"`
	SimplexPages *int64 `json:"simplex_pages"`
	DuplexPages  *int64 `json:"duplex_pages"`
	ScanPages    *int64 `json:"scan_pages"`
}

type Supply struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Color  string `json:"color"`
	Level  *int   `json:"level_percent"`
	Status string `json:"status"`
}

type Alert struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Level   string `json:"level"`
}

type Metrics struct {
	PollStartedAt   string  `json:"poll_started_at"`
	PollCompletedAt string  `json:"poll_completed_at"`
	PollDurationMs  int64   `json:"poll_duration_ms"`
	SuccessRate     float64 `json:"success_rate"`
}

func New(agentID, hostname, version string) *Payload {
	now := time.Now().UTC().Format(time.RFC3339)
	return &Payload{
		Source: Source{
			AgentID:          agentID,
			Hostname:         hostname,
			OS:               "windows",
			Version:          version,
			CollectionMethod: "usb_direct",
			ConnectedVia:     "usb",
			Timestamp:        now,
		},
		Printer:      Printer{Trays: []Tray{}},
		Counters:     Counters{},
		Supplies:     []Supply{},
		DeviceAlerts: []Alert{},
		Metrics:      Metrics{PollStartedAt: now},
	}
}

func Int64Ptr(v int64) *int64 { return &v }
func StrPtr(v string) *string { return &v }
