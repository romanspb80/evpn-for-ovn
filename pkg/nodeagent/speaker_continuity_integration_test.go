package nodeagent

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestSpeakerContinuityReconnect_Live(t *testing.T) {
	if os.Getenv("EVPN_EXPORT_LIVE") != "disposable" {
		t.Skip("requires disposable node-local speaker and shared PID namespace")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	guard := &unixSpeakerContinuity{}
	defer guard.Close()
	var first SpeakerProcessEvidence
	for i := 0; i < 2; i++ {
		conn, err := grpc.NewClient("unix:///var/run/gobgp/gobgp.sock",
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return guard.DialContext(ctx, "unix:///var/run/gobgp/gobgp.sock")
			}))
		if err != nil {
			t.Fatal(err)
		}
		global, err := api.NewGoBgpServiceClient(conn).GetBgp(ctx, &api.GetBgpRequest{})
		closeErr := conn.Close()
		if err != nil || closeErr != nil || global.GetGlobal().GetAsn() == 0 {
			t.Fatalf("real reconnect: %v %v %v", global, err, closeErr)
		}
		if err := guard.Check(ctx); err != nil {
			t.Fatal(err)
		}
		evidence := guard.Evidence()
		if i == 0 {
			first = evidence
		} else if evidence != first {
			t.Fatal("speaker changed during reconnect")
		}
		t.Logf("fresh real gRPC connection %d kernel peer=%+v", i, evidence)
	}
}
