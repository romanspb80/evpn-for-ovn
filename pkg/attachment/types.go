package attachment

import "github.com/romanspb80/evpn-for-ovn/pkg/endpoint"

type LocalityMode string

const (
	LocalAttachEnabled  LocalityMode = "enabled"
	LocalAttachDisabled LocalityMode = "disabled"
)

type Config struct {
	Mode LocalityMode
}

type EnsureResult = endpoint.AttachmentStatus
