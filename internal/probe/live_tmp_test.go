package probe

// Temporary live verification — not committed. Drives the real engine at the
// mirrors the production panel reports as "needing credentials".

import (
	"fmt"
	"testing"
	"time"

	"github.com/DC1024/mirrorpilot/internal/mirror"
)

func TestLiveTokenRealmFix(t *testing.T) {
	if testing.Short() {
		t.Skip("live")
	}
	target := Target{Repository: "library/alpine", Reference: "latest"}
	p, err := New(Options{Target: target, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{
		"https://docker.m.daocloud.io",
		"https://docker.1ms.run",
		"https://hub.rat.dev",
		"https://docker.1panel.live",
		"https://docker.nju.edu.cn",
	} {
		res := p.Probe(t.Context(), mirror.Source{ID: "live", Name: url, URL: url})
		fmt.Printf("%-32s status=%-12s connect=%s token=%s manifest=%s throughput=%s detail=%q\n",
			url, res.Status, res.Connect.Status, res.Token.Status, res.Manifest.Status, res.Throughput.Status, res.Detail)
	}
}
