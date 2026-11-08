//go:build linux

package nodeagent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpointauth"
	"github.com/romanspb80/evpn-for-ovn/pkg/ownership"
)

const endpointAuthorizationFenceOperation = "endpoint-authorization-fence"

type endpointAuthorizationJournalStore struct {
	journal *ownership.Journal
}

// OpenEndpointAuthorizationFencer opens the authorization-only durable state
// in a dedicated directory. It must not share the exporter route-receipt
// directory. Its stable scope deliberately survives host reboot.
func OpenEndpointAuthorizationFencer(
	stateDirectory, installationUID, nodeUID string,
) (*endpointauth.Fencer, error) {
	domain, err := endpointAuthorizationFenceDomain(installationUID, nodeUID)
	if err != nil {
		return nil, err
	}
	journal, err := ownership.Open(stateDirectory, domain, true)
	if err != nil {
		return nil, err
	}
	store := &endpointAuthorizationJournalStore{journal: journal}
	return endpointauth.OpenFencer(store, installationUID, nodeUID)
}

func endpointAuthorizationFenceDomain(installationUID, nodeUID string) (ownership.Domain, error) {
	if !validFenceScopeIdentity(installationUID) || !validFenceScopeIdentity(nodeUID) {
		return ownership.Domain{}, fmt.Errorf("installation and node identities are required for authorization fencing")
	}
	scopeData, err := json.Marshal([]string{installationUID, nodeUID})
	if err != nil {
		return ownership.Domain{}, fmt.Errorf("encode authorization fence scope: %w", err)
	}
	scopeDigest := sha256.Sum256(scopeData)
	installation := uuid.NewSHA1(uuid.NameSpaceOID, []byte("installation:"+installationUID))
	node := uuid.NewSHA1(uuid.NameSpaceOID, []byte("node:"+nodeUID))
	// Authorization history spans boots, unlike the route-owner process scope.
	stableBootScope := uuid.NewSHA1(uuid.NameSpaceOID, []byte("endpoint-authorization-fence-v1"))
	configDigest := sha256.Sum256([]byte("evpn-endpoint-authorization-fence-v1"))
	return ownership.Domain{
		InstallationUID: installation.String(),
		NodeUID:         node.String(),
		BootID:          stableBootScope.String(),
		NetworkNamespace: "endpoint-authorization:" +
			hex.EncodeToString(scopeDigest[:]),
		ConfigDigest: hex.EncodeToString(configDigest[:]),
	}, nil
}

func validFenceScopeIdentity(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (s *endpointAuthorizationJournalStore) Load() ([]byte, error) {
	state, pending, err := s.journal.Snapshot()
	if err != nil {
		return nil, err
	}
	if pending != nil {
		if pending.Operation != endpointAuthorizationFenceOperation || len(pending.Payload) == 0 {
			return nil, fmt.Errorf("%w: unresolved non-authorization operation in fence journal", ownership.ErrBlocked)
		}
		if !json.Valid(pending.Payload) {
			return nil, fmt.Errorf("%w: invalid pending authorization fence state", ownership.ErrBlocked)
		}
		if err := s.journal.Commit(pending.Sequence, pending.Payload); err != nil {
			return nil, err
		}
		state = append(json.RawMessage(nil), pending.Payload...)
	}
	if string(state) == "null" {
		return nil, endpointauth.ErrFenceStateNotFound
	}
	return append([]byte(nil), state...), nil
}

func (s *endpointAuthorizationJournalStore) Persist(state []byte) error {
	if len(state) == 0 || !json.Valid(state) {
		return fmt.Errorf("%w: invalid authorization fence state", ownership.ErrBlocked)
	}
	sequence, err := s.journal.Begin(endpointAuthorizationFenceOperation, json.RawMessage(state))
	if err != nil {
		return err
	}
	return s.journal.Commit(sequence, json.RawMessage(state))
}

func (s *endpointAuthorizationJournalStore) Close() error {
	return s.journal.Close()
}
