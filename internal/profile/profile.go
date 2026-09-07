package profile

import (
	"fmt"
	"usb-agent/internal/config"
	"usb-agent/internal/discovery"
	"usb-agent/internal/payload"
)

// Logger abstracts logging for profiles, preventing circular dependencies.
type Logger interface {
	Log(msg string)
	Logf(format string, args ...interface{})
}

// PrinterProfile defines a specific extraction strategy for a printer brand or family.
type PrinterProfile interface {
	// Name returns the human-readable name of the profile.
	Name() string

	// Match returns true if this profile is capable of handling the provided printer.
	Match(printer discovery.USBPrinter) bool

	// Extract executes the profile's specific extraction logic and populates the payload.
	// Returns a boolean indicating if extraction was successful, and an error if one occurred.
	Extract(printer discovery.USBPrinter, cfg *config.Config, p *payload.Payload, lg Logger) (bool, error)
}

// BasicLogger implements Logger using a provided log function.
type BasicLogger struct {
	LogFn func(string)
}

func (l *BasicLogger) Log(msg string) {
	if l.LogFn != nil {
		l.LogFn(msg)
	}
}

func (l *BasicLogger) Logf(format string, args ...interface{}) {
	if l.LogFn != nil {
		l.LogFn(fmt.Sprintf(format, args...))
	}
}

// Registry holds all available printer profiles.
var Registry = []PrinterProfile{}

// Register adds a new profile to the active registry.
func Register(p PrinterProfile) {
	Registry = append(Registry, p)
}
