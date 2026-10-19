package omniroute

import (
	"net/http"
	"testing"
	"time"
)

func TestLimitHeaders(t *testing.T) {
	now := time.Unix(1000, 0)
	h := http.Header{}
	h.Set("x-ratelimit-limit-requests", "60")
	h.Set("x-ratelimit-remaining-requests", "59")
	h.Set("x-ratelimit-reset-requests", "1s")
	h.Set("anthropic-ratelimit-tokens-remaining", "500")
	tr := NewLimitTracker()
	tr.Observe("p", h, 200, now)
	rl := tr.Snapshot()["p"]
	if rl.ReqLimit != 60 || rl.ReqRemaining != 59 || !rl.ReqReset.Equal(now.Add(time.Second)) || rl.TokRemaining != 500 {
		t.Fatalf("неверный разбор: %+v", rl)
	}
}

func TestDetectCLILimit(t *testing.T) {
	if ok, d := DetectCLILimit("Error: usage limit reached, try again in 5 minutes"); !ok || d != 5*time.Minute {
		t.Fatalf("got %v %v", ok, d)
	}
	if ok, _ := DetectCLILimit("all good"); ok {
		t.Fatal("ложное срабатывание")
	}
}
