//go:build integration

package edge

import (
	"context"
	"testing"
	"time"

	cedge "github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/test/integration/itest"
)

func TestZZZCountsAgreeWithTheClient(t *testing.T) {
	begin(t)
	needRepo(t)

	since := itest.Scale(2 * time.Minute)
	base := edgeCounts(t, since)
	beforeHost, _ := base.Host(hostX)
	t.Logf("before: %d request(s) on %s", beforeHost.Requests, hostX)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(3*time.Minute))
	defer cancel()

	const requests = 40
	client := internet.Client(0)
	defer client.CloseIdleConnections()
	ok := 0
	for i := 0; i < requests; i++ {
		res, err := internet.Do(ctx, client, urlX+"/")
		if err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
		if res.Status == 200 {
			ok++
		}
	}
	if ok != requests {
		t.Fatalf("%d of %d requests answered 200; this case needs a healthy pool", ok, requests)
	}

	time.Sleep(itest.Scale(2 * time.Second))

	after := edgeCounts(t, since)
	host, found := after.Host(hostX)
	if !found {
		t.Fatalf("the edge counted nothing for %s: %+v", hostX, after.Hosts)
	}
	served := host.Requests - beforeHost.Requests
	t.Logf("after: %d request(s) on %s (%d in this window), %d error(s) over %d target(s)",
		host.Requests, hostX, served, host.Errors(), len(host.Targets))

	if served < requests {
		t.Errorf("the edge counted %d request(s) for %d made: a watch that undercounts "+
			"would promote a release that is failing", served, requests)
	}
	if host.Errors() != 0 {
		t.Errorf("the edge counted %d error(s) on %d good responses: a watch that overcounts "+
			"would roll back a release that is fine", host.Errors(), requests)
	}

	t.Run("and per replica, summing to the host", func(t *testing.T) {
		if len(host.Targets) < 2 {
			t.Fatalf("%s has %d target(s); this app has two replicas", hostX, len(host.Targets))
		}
		total := 0
		for _, target := range host.Targets {
			t.Logf("  replica %d: %d request(s), %d 5xx, %d refused",
				target.Replica, target.Requests, target.Status5xx, target.ConnectFailures)
			total += target.Requests
		}
		if total != host.Requests {
			t.Errorf("the replicas add up to %d and the host says %d", total, host.Requests)
		}
		for _, target := range host.Targets {
			if target.Requests == 0 {
				t.Errorf("replica %d counted nothing while the pool round-robins", target.Replica)
			}
		}
	})

	t.Run("a name the machine does not serve has no counts", func(t *testing.T) {
		if _, found := after.Host(hostUnknown); found {
			t.Errorf("the edge counts %s, which it has never routed", hostUnknown)
		}
	})
}

func edgeCounts(t *testing.T, since time.Duration) cedge.Counts {
	t.Helper()
	res := mustInRepo(t, "edge", "counts", "--since", since.String(), "--json")
	return decode[cedge.Counts](t, "edge counts", res.Stdout)
}
