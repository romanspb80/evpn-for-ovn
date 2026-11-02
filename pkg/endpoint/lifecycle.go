package endpoint

import (
	"fmt"
	"strings"
)

func NewEndpointID(networkID, attachmentID, workloadUID string) EndpointID {
	return EndpointID(fmt.Sprintf("%s|%s|%s", strings.TrimSpace(networkID), strings.TrimSpace(attachmentID), strings.TrimSpace(workloadUID)))
}
