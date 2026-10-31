package swap

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestParseClosedPairs(t *testing.T) {
	t.Parallel()
	got, err := ParseClosedPairs(" arg>uarg , WARG>Uarg,")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[[2]string{ARG, UARG}] || !got[[2]string{WARG, UARG}] {
		t.Fatalf("parsed %v", got)
	}
	if got, err := ParseClosedPairs(""); err != nil || got != nil {
		t.Fatalf("empty: %v, %v", got, err)
	}
	for _, bad := range []string{"ARG>ARG", "ARG>BTC", "ARG", "ARG>UARG>WARG", ">UARG"} {
		if _, err := ParseClosedPairs(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

// With ARG>UARG closed, info leaves out that one direction, a quote for it
// is refused with 503, and the other directions keep quoting.
func TestClosedPairs(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	e.m.cfg.ClosedPairs = map[[2]string]bool{{ARG, UARG}: true}

	info, err := e.c.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Closed != "" {
		t.Fatalf("info says closed: %q", info.Closed)
	}
	listed := map[[2]string]bool{}
	for _, p := range info.Pairs {
		listed[[2]string{p.From, p.To}] = true
	}
	if listed[[2]string{ARG, UARG}] {
		t.Error("info lists the closed ARG>UARG")
	}
	for _, want := range [][2]string{{UARG, ARG}, {ARG, WARG}} {
		if !listed[want] {
			t.Errorf("info does not list %s>%s", want[0], want[1])
		}
	}
	if len(info.Pairs) != 5 {
		t.Errorf("%d pairs listed, want 5", len(info.Pairs))
	}

	userCoins, _ := e.user.SpendableCoins("")
	recv, _ := e.user.ReceiveAddress()
	chg, _ := e.user.ChangeAddress()
	req := &QuoteRequest{From: "arg", To: "uarg", Amount: unit, Inputs: []Outpoint{{TxID: userCoins[0].TxID, Index: userCoins[0].Index}}, Receive: recv, Change: chg}
	body, _ := json.Marshal(req)
	resp, err := http.Post(e.srv.URL+"/api/quote", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var ans struct {
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&ans)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || ans.Error != "This direction is closed for now" {
		t.Fatalf("quote for the closed ARG>UARG: %d %q", resp.StatusCode, ans.Error)
	}
	if len(e.m.quotes) != 0 {
		t.Fatalf("%d quotes held after a refused one", len(e.m.quotes))
	}

	// The other way round still quotes, and so does wrapping.
	assets := e.assets()
	if _, err := Prepare(ctx, e.user, e.c, assets, UARG, ARG, unit, 0); err != nil {
		t.Fatalf("UARG>ARG: %v", err)
	}
	if _, err := Prepare(ctx, e.user, e.c, assets, ARG, WARG, unit, 0); err != nil {
		t.Fatalf("ARG>WARG: %v", err)
	}
}
