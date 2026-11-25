package logs

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestRelayFollowsTargetAndKeepsAttributes(t *testing.T) {
	var first, second bytes.Buffer

	target := NewTarget(slog.NewTextHandler(&first, nil))
	logger := slog.New(NewRelay(target, false)).With("peer", "192.0.2.1").WithGroup("g")
	logger.Info("one", "k", 1)
	target.Set(slog.NewTextHandler(&second, nil))
	logger.Info("two", "k", 2)

	if !strings.Contains(first.String(), "msg=one peer=192.0.2.1 g.k=1") || strings.Contains(first.String(), "two") {
		t.Fatalf("first target got %q", first.String())
	}

	if !strings.Contains(second.String(), "msg=two peer=192.0.2.1 g.k=2") {
		t.Fatalf("second target got %q", second.String())
	}
}

func TestRelayDemotesInfo(t *testing.T) {
	var output bytes.Buffer

	logger := slog.New(NewRelay(NewTarget(slog.NewTextHandler(&output, nil)), true))
	logger.Info("hidden")
	logger.Warn("shown")
	if strings.Contains(output.String(), "hidden") || !strings.Contains(output.String(), "shown") {
		t.Fatalf("got %q", output.String())
	}
}
