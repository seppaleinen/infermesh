package capabilities

import (
	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// CapabilitiesProvider defines the interface for retrieving worker capabilities.
type CapabilitiesProvider interface {
	Get() (protocol.Capabilities, error)
}