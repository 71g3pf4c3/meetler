package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/twitchtv/twirp"
)

func pokerCards(text string) []int {
	var cards []int
	for _, card := range strings.Fields(text) {
		cards = append(cards, strings.IndexByte("shdc", card[1])*13+strings.IndexByte("23456789TJQKA", card[0]))
	}
	return cards
}

func TestPokerHandRanking(t *testing.T) {
	hands := []string{
		"As Kd 9c 6h 3s 2d 8h",
		"As Ad Kc Qh 9s 2d 8h",
		"As Ad Kc Kh 9s 2d 8h",
		"As Ad Ac Kh 9s 2d 8h",
		"As 2d 3c 4h 5s Qd 8h",
		"As Js 9s 6s 3s Qd 8h",
		"As Ad Ac Kh Ks 2d 8h",
		"As Ad Ac Ah Ks 2d 8h",
		"As Ks Qs Js Ts 2d 8h",
	}
	last := -1
	for category, hand := range hands {
		value := pokerValue(pokerCards(hand))
		if value <= last || value/759375 != category {
			t.Fatalf("hand %s: score %d, category %d", hand, value, category)
		}
		last = value
	}
	for _, pair := range [][2]string{
		{"As Ad Kc Qh 9s 2d 8h", "As Ad Kc Jh 9s 2d 8h"},
		{"2s 3d 4c 5h 6s Qd 8h", "As 2d 3c 4h 5s Qd 8h"},
		{"As Ad Ac Kh Ks Kd 8h", "Ks Kd Kc Ah As 2d 8h"},
		{"As Ad Kc Kh Qs 2d 8h", "As Ad Qc Qh Ks 2d 8h"},
	} {
		if pokerValue(pokerCards(pair[0])) <= pokerValue(pokerCards(pair[1])) {
			t.Fatalf("%s must beat %s", pair[0], pair[1])
		}
	}
}

func newPokerTestTable(t *testing.T, stacks ...int) *pokerTable {
	t.Helper()
	table := &pokerTable{dealer: -1, turn: -1, street: "waiting", minRaise: pokerBigBlind}
	for i, chips := range stacks {
		table.players[i] = &pokerPlayer{key: strings.Repeat(string(rune('a'+i)), 16), name: string(rune('A' + i)), chips: chips}
	}
	if err := table.deal(time.Now()); err != nil {
		t.Fatal(err)
	}
	return table
}

func pokerTestAction(t *testing.T, table *pokerTable, action string, amount int) {
	t.Helper()
	if table.turn < 0 {
		t.Fatal("no player to act")
	}
	key, now := table.players[table.turn].key, time.Now()
	var err error
	switch action {
	case "fold":
		err = table.fold(key, now)
	case "check":
		err = table.check(key, now)
	case "call":
		err = table.call(key, now)
	case "raise":
		err = table.raise(key, amount, now)
	default:
		t.Fatalf("unknown test move %s", action)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestPokerHeadsUpAndCheckdown(t *testing.T) {
	table := newPokerTestTable(t, 1000, 1000)
	if table.dealer != 0 || table.turn != 0 || table.players[0].bet != 5 || table.players[1].bet != 10 {
		t.Fatalf("heads-up blinds/turn: %+v", table)
	}
	seen := map[int]bool{}
	for _, p := range table.players {
		if p != nil {
			for _, card := range p.cards {
				if seen[card] {
					t.Fatal("duplicate dealt card")
				}
				seen[card] = true
			}
		}
	}
	pokerTestAction(t, table, "call", 0)
	if table.turn != 1 || table.street != "preflop" {
		t.Fatal("big blind must have an option")
	}
	pokerTestAction(t, table, "check", 0)
	if table.street != "flop" || len(table.board) != 3 || table.turn != 1 {
		t.Fatal("non-dealer acts first after the flop")
	}
	for table.active() {
		pokerTestAction(t, table, "check", 0)
	}
	if len(table.board) != 5 || !table.reveal || table.players[0].chips+table.players[1].chips != 2000 {
		t.Fatalf("invalid showdown: %+v", table)
	}
	if err := table.deal(time.Now()); err != nil || table.dealer != 1 || table.turn != 1 {
		t.Fatal("dealer must rotate")
	}
}

func TestPokerFoldAndPrivateCards(t *testing.T) {
	table := newPokerTestTable(t, 1000, 1000, 1000)
	for i, p := range table.players[:3] {
		view := table.view(p.key)
		for j, seat := range view.Seats[:3] {
			if (len(seat.Cards) == 2) != (i == j) {
				t.Fatalf("private cards exposed to seat %d", i)
			}
		}
	}
	for _, p := range table.view("").Seats {
		if p != nil && (len(p.Cards) != 0 || p.You) {
			t.Fatal("spectator can see private cards")
		}
	}
	pokerTestAction(t, table, "raise", 50)
	pokerTestAction(t, table, "fold", 0)
	pokerTestAction(t, table, "fold", 0)
	if table.active() || table.reveal || table.players[0].chips != 1015 {
		t.Fatal("uncontested pot payout failed")
	}
	for _, p := range table.view("").Seats {
		if p != nil && len(p.Cards) > 0 {
			t.Fatal("uncontested winner must keep their cards private")
		}
	}
}

func TestPokerAllInSidePots(t *testing.T) {
	table := newPokerTestTable(t, 100, 200, 300)
	table.players[0].cards = pokerCards("As Ad")
	table.players[1].cards = pokerCards("Ks Kd")
	table.players[2].cards = pokerCards("Qs Qd")
	table.deck = pokerCards("2c 3h 7s 9d 4c Jh 5c Th")
	pokerTestAction(t, table, "raise", 100)
	pokerTestAction(t, table, "raise", 200)
	pokerTestAction(t, table, "call", 0)
	if table.active() || !table.reveal || len(table.board) != 5 {
		t.Fatal("all-in runout failed")
	}
	for i, want := range []int{300, 200, 100} {
		if table.players[i].chips != want {
			t.Fatalf("seat %d has %d chips, want %d", i, table.players[i].chips, want)
		}
	}
}

func TestPokerSplitPotAndOddChip(t *testing.T) {
	table := &pokerTable{dealer: 0, street: "river", board: pokerCards("As Ks Qs Js Ts"), pot: 15}
	for i := range 3 {
		table.players[i] = &pokerPlayer{name: "player", cards: pokerCards("2c 3d"), total: 5, inHand: true, folded: i == 2}
	}
	table.finish(true)
	if table.players[0].chips != 7 || table.players[1].chips != 8 || table.players[2].chips != 0 {
		t.Fatal("odd chip should go clockwise from the dealer")
	}
}

func TestPokerShortAllInDoesNotReopenRaise(t *testing.T) {
	table := newPokerTestTable(t, 1000, 1000, 15)
	pokerTestAction(t, table, "call", 0)
	pokerTestAction(t, table, "call", 0)
	pokerTestAction(t, table, "raise", 15)
	if table.turn != 0 || table.canRaise(table.players[0]) {
		t.Fatal("short all-in must not reopen a completed action")
	}
	before := table.players[0].chips
	if err := table.raise(table.players[0].key, 25, time.Now()); err == nil {
		t.Fatal("illegal re-raise accepted")
	}
	if table.players[0].chips != before {
		t.Fatal("rejected raise moved chips")
	}
	pokerTestAction(t, table, "call", 0)
	pokerTestAction(t, table, "call", 0)
	if table.street != "flop" {
		t.Fatal("short raise was not fully called")
	}
}

func TestPokerLastAction(t *testing.T) {
	table := newPokerTestTable(t, 1000, 1000)
	if table.players[0].lastAction != "smallBlind" || table.players[0].lastAmount != 5 || table.players[1].lastAction != "bigBlind" {
		t.Fatal("blinds were not recorded")
	}
	pokerTestAction(t, table, "call", 0)
	pokerTestAction(t, table, "check", 0)
	if table.street != "flop" || table.players[0].lastAction != "call" || table.players[0].lastAmount != 5 {
		t.Fatal("advancing the street lost the last action")
	}
	pokerTestAction(t, table, "raise", 20)
	if table.players[1].lastAction != "bet" || table.players[1].lastAmount != 20 {
		t.Fatal("opening bet was not recorded")
	}
	pokerTestAction(t, table, "raise", 60)
	pokerTestAction(t, table, "fold", 0)
	view := table.view(table.players[0].key)
	if view.Seats[0].LastAction != "raise" || view.Seats[0].LastAmount != 60 || view.Seats[1].LastAction != "fold" {
		t.Fatal("finished hand lost its last actions")
	}
	if err := table.deal(time.Now()); err != nil {
		t.Fatal(err)
	}
	if table.players[0].lastAction != "bigBlind" || table.players[0].lastAmount != 10 {
		t.Fatal("new hand retained an old action")
	}
}

func TestPokerNetResult(t *testing.T) {
	table := newPokerTestTable(t, 1000, 1000)
	if table.view("").Seats[0].Net != nil {
		t.Fatal("unfinished hand has a final result")
	}
	pokerTestAction(t, table, "fold", 0)
	view := table.view("")
	if *view.Seats[0].Net != -5 || *view.Seats[1].Net != 5 {
		t.Fatal("folded hand did not report net results")
	}
	if err := table.deal(time.Now()); err != nil {
		t.Fatal(err)
	}
	view = table.view("")
	if view.Seats[0].Net != nil || view.Seats[1].Net != nil {
		t.Fatal("new hand retained old result badges")
	}
	pokerTestAction(t, table, "fold", 0)
	if err := table.deal(time.Now()); err != nil {
		t.Fatal(err)
	}
	table.board = pokerCards("As Ks Qs Js Ts")
	table.pot = 20
	for _, p := range table.players[:2] {
		p.chips, p.total, p.bet = p.chips+p.total-10, 10, 10
		p.cards = pokerCards("2c 3d")
	}
	table.finish(true)
	view = table.view("")
	for _, p := range view.Seats[:2] {
		if p.Net == nil || *p.Net != 0 {
			t.Fatal("an even split must report zero net")
		}
	}
	data, err := json.Marshal(view)
	if err != nil || !strings.Contains(string(data), `"net":0`) {
		t.Fatal("zero results were omitted from JSON")
	}
}

func TestPokerReturnedBetIsNotAWin(t *testing.T) {
	table := newPokerTestTable(t, 100, 200)
	table.board = pokerCards("2c 3h 7s 9d 4c")
	table.players[0].cards = pokerCards("As Ad")
	table.players[1].cards = pokerCards("Ks Kd")
	for i, amount := range []int{100, 200} {
		p := table.players[i]
		p.chips, p.bet, p.total = 0, amount, amount
	}
	table.pot = 300
	table.finish(true)
	view := table.view("")
	if *view.Seats[0].Net != 100 || *view.Seats[1].Net != -100 || strings.Contains(table.result, table.players[1].name) {
		t.Fatal("returned unmatched bet was mistaken for a win")
	}
}

func TestPokerLeaveKeepsContributions(t *testing.T) {
	table := newPokerTestTable(t, 1000, 1000, 1000)
	pokerTestAction(t, table, "raise", 50)
	turn := table.turn
	if err := table.leave(table.players[0].key, time.Now()); err != nil {
		t.Fatal(err)
	}
	if table.turn != turn || table.players[0] != nil || table.pot != 65 {
		t.Fatal("leave must free the seat and retain the pot and current turn")
	}
	pokerTestAction(t, table, "fold", 0)
	if table.active() || table.players[0] != nil || table.players[2].chips != 1055 {
		t.Fatal("leaving player's contribution was lost")
	}
}

func TestPokerLastPlayerLeavingResetsTable(t *testing.T) {
	table := newPokerTestTable(t, 1000, 1000)
	pokerTestAction(t, table, "call", 0)
	pokerTestAction(t, table, "check", 0)
	if len(table.board) != 3 || table.pot == 0 {
		t.Fatal("expected a hand with community cards and a pot")
	}
	now := time.Now()
	for _, p := range table.players {
		if p != nil {
			if err := table.leave(p.key, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	view := table.view("")
	if view.Street != "waiting" || len(view.Board) != 0 || view.Pot != 0 || view.Result != "" || view.Turn != -1 || view.Deadline != 0 {
		t.Fatalf("empty table retained the old hand: %+v", view)
	}
	for i, key := range []string{"new-player-aaaaaaaa", "new-player-bbbbbbbb"} {
		if err := table.sit(key, i, "New player", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := table.startHand("new-player-aaaaaaaa", now); err != nil {
		t.Fatal(err)
	}
	if table.street != "preflop" || len(table.board) != 0 || table.pot != 15 {
		t.Fatal("new players inherited the previous hand")
	}
}

func TestPokerCumulativeShortRaisesReopenAction(t *testing.T) {
	table := newPokerTestTable(t, 1000, 15, 20, 1000)
	pokerTestAction(t, table, "call", 0)
	pokerTestAction(t, table, "call", 0)
	pokerTestAction(t, table, "raise", 15)
	pokerTestAction(t, table, "raise", 20)
	if table.turn != 3 || !table.canRaise(table.players[3]) {
		t.Fatal("cumulative short raises must reopen a full raise")
	}
	pokerTestAction(t, table, "raise", 30)
}

func TestPokerTinyStacksRunOut(t *testing.T) {
	for _, stacks := range [][]int{{3, 4}, {5, 5}, {1, 1, 1}, {1000, 3}} {
		table := newPokerTestTable(t, stacks...)
		for table.active() {
			pokerTestAction(t, table, "call", 0)
		}
		want, got := 0, 0
		for i, stack := range stacks {
			want += stack
			got += table.players[i].chips
		}
		if got != want {
			t.Fatalf("stacks %v: got %d chips, want %d", stacks, got, want)
		}
	}
}

func TestPokerHTTP(t *testing.T) {
	store := newPokerStore(nil)
	key := "aaaaaaaaaaaaaaaa"
	request := func(method, endpoint, key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/rooms/test/poker"+endpoint, strings.NewReader(body))
		r.Header.Set("X-Poker-Player", key)
		w := httptest.NewRecorder()
		store.routes.ServeHTTP(w, r)
		return w
	}
	if w := request("POST", "/seats", "", `{"seat":0}`); w.Code != 400 {
		t.Fatal("missing player key accepted")
	}
	if w := request("POST", "/seats", key, `{"seat":0,"name":"Alice","identity":"anon-alice"}`); w.Code != 204 || w.Body.Len() != 0 {
		t.Fatal(w.Body.String())
	}
	if w := request("POST", "/seats", "bbbbbbbbbbbbbbbb", `{"seat":0,"identity":"anon-bob"}`); w.Code != 409 {
		t.Fatal("occupied seat accepted")
	}
	if w := request("POST", "/seats", "bbbbbbbbbbbbbbbb", `{"seat":1,"name":"Bob","identity":"anon-bob"}`); w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	if w := request("POST", "/deal", key, ""); w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	var own, spectator pokerView
	if err := json.Unmarshal(request("GET", "", key, "").Body.Bytes(), &own); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(request("GET", "", "", "").Body.Bytes(), &spectator); err != nil {
		t.Fatal(err)
	}
	if len(own.Seats[0].Cards) != 2 || len(spectator.Seats[0].Cards) != 0 {
		t.Fatal("HTTP hole-card privacy failed")
	}
	table := store.tables["test"]
	oldTurn := table.turn
	table.deadline = time.Now().Add(-time.Second)
	request("GET", "", key, "")
	if table.active() || table.turn == oldTurn {
		t.Fatal("expired turn did not fold")
	}
	data := request("GET", "", "", "").Body.String()
	if strings.Contains(data, key) {
		t.Fatal("player key leaked in table response")
	}
	if w := request("POST", "", key, `{"action":"sit"}`); w.Code != 405 {
		t.Fatal("generic action endpoint is still accepted")
	}
	if w := request("POST", "/raise", key, `{`); w.Code != 400 {
		t.Fatal("malformed raise request accepted")
	}
	if w := request("DELETE", "/seats/me", key, ""); w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	if store.tables["test"].players[0] != nil {
		t.Fatal("deleted seat is still occupied")
	}
	if w := request("DELETE", "/seats/me", key, ""); w.Code != 204 {
		t.Fatal("leaving should be idempotent")
	}
	store.clearRoom("test")
	if len(store.tables) != 0 {
		t.Fatal("deleted room retained its poker table")
	}
}

type pokerTestRoomClient struct {
	participants []*livekit.ParticipantInfo
	err          error
}

func (c *pokerTestRoomClient) ListParticipants(_ context.Context, _ *livekit.ListParticipantsRequest) (*livekit.ListParticipantsResponse, error) {
	return &livekit.ListParticipantsResponse{Participants: c.participants}, c.err
}

func TestPokerRemovesDisconnectedParticipantWithoutLeaveRequest(t *testing.T) {
	client := &pokerTestRoomClient{participants: []*livekit.ParticipantInfo{
		{Identity: "anon-alice"}, {Identity: "anon-bob"},
	}}
	store := newPokerStore(client)
	request := func(method, endpoint, key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/rooms/test/poker"+endpoint, strings.NewReader(body))
		r.Header.Set("X-Poker-Player", key)
		w := httptest.NewRecorder()
		store.routes.ServeHTTP(w, r)
		if w.Code >= 400 {
			t.Fatal(w.Body.String())
		}
		return w
	}
	const alice, bob = "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"
	request("POST", "/seats", alice, `{"seat":0,"name":"Alice","identity":"anon-alice"}`)
	request("POST", "/seats", bob, `{"seat":1,"name":"Bob","identity":"anon-bob"}`)
	request("POST", "/deal", alice, "")
	request("GET", "", bob, "")
	table := store.tables["test"]
	if table.players[0] == nil || table.players[1] == nil {
		t.Fatal("connected participants lost their seats")
	}
	// No DELETE or unload request: LiveKit is the source of the departure.
	client.participants = client.participants[1:]
	var view pokerView
	if err := json.Unmarshal(request("GET", "", bob, "").Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Seats[0] != nil || view.Seats[1] == nil || table.active() || table.players[1].chips != 1005 {
		t.Fatal("departed participant retained a seat or their blind was lost")
	}
	client.err = twirp.NewError(twirp.Unavailable, "temporarily unavailable")
	request("GET", "", bob, "")
	if table.players[1] == nil {
		t.Fatal("LiveKit request failure removed a connected participant")
	}
	client.err = twirp.NotFoundError("room closed")
	request("GET", "", "", "")
	if table.players[1] != nil {
		t.Fatal("deleted LiveKit room retained a seat")
	}
	if table.pot != 0 || table.street != "waiting" {
		t.Fatal("departed LiveKit participants left an old hand behind")
	}
}

func TestPokerSeatCanBeReusedDuringHand(t *testing.T) {
	table := newPokerTestTable(t, 1000, 1000, 1000, 1000)
	pokerTestAction(t, table, "raise", 50)
	key := table.players[3].key
	if err := table.leave(key, time.Now()); err != nil {
		t.Fatal(err)
	}
	if table.players[3] != nil || table.seat(key) >= 0 || table.pot != 65 {
		t.Fatal("departed player remains seated or their bet was lost")
	}
	if err := table.sit("new-player-123456", 3, "New player", time.Now()); err != nil {
		t.Fatal(err)
	}
	if p := table.players[3]; p.inHand || len(p.cards) > 0 {
		t.Fatal("new arrival inherited a departed player's hand")
	}
	for table.active() {
		pokerTestAction(t, table, "call", 0)
	}
	if table.players[3].chips != pokerBuyIn {
		t.Fatal("new arrival participated in the previous player's pot")
	}
}

func TestPokerDepartedPlayerSidePot(t *testing.T) {
	table := newPokerTestTable(t, 100, 200, 300, 400)
	table.players[0].cards = pokerCards("As Ad")
	table.players[1].cards = pokerCards("Ks Kd")
	table.players[2].cards = pokerCards("Qs Qd")
	table.players[3].cards = pokerCards("Js Jd")
	table.deck = pokerCards("2c 3h 7s 9d 4c Th 5c 5h")
	pokerTestAction(t, table, "raise", 100)
	pokerTestAction(t, table, "call", 0)
	pokerTestAction(t, table, "raise", 200)
	pokerTestAction(t, table, "call", 0)
	pokerTestAction(t, table, "call", 0)
	if err := table.leave(table.players[2].key, time.Now()); err != nil {
		t.Fatal(err)
	}
	if table.active() || table.players[2] != nil || table.pot != 700 {
		t.Fatal("departed player's all-in hand did not settle")
	}
	if table.players[0].chips != 400 || table.players[1].chips != 300 || table.players[3].chips != 200 {
		t.Fatal("departed player's contribution was omitted from side pots")
	}
}

func TestPokerIdleSeatsExpire(t *testing.T) {
	table := newPokerTestTable(t, 1000, 1000, 1000)
	now := time.Now()
	for _, p := range table.players {
		if p != nil {
			p.seen = now
		}
	}
	table.players[1].seen = now.Add(-pokerSeatTTL - time.Second)
	table.expire(now)
	if table.players[1] != nil || table.players[0] == nil || table.players[2] == nil {
		t.Fatal("inactive seat cleanup removed the wrong players")
	}
	if table.pot != 15 || len(table.deadBets) != 1 || table.deadBets[0] != 5 {
		t.Fatal("inactive player's blind was lost")
	}
}

func TestPokerCompleteGamesConserveChips(t *testing.T) {
	for range 25 {
		table := newPokerTestTable(t, 1000, 1000, 1000, 1000, 1000, 1000)
		for hand := 0; hand < 10; hand++ {
			for steps := 0; table.active() && steps < 10000; steps++ {
				p := table.players[table.turn]
				if table.canRaise(p) && (steps+hand)%4 == 0 {
					pokerTestAction(t, table, "raise", min(p.chips+p.bet, table.bet+table.minRaise))
				} else {
					pokerTestAction(t, table, "call", 0)
				}
			}
			if table.active() {
				t.Fatal("betting round did not terminate")
			}
			total := 0
			for _, p := range table.players {
				total += p.chips
			}
			if total != 6000 || table.pot < 0 {
				t.Fatalf("chip conservation failed: %d", total)
			}
			if table.count(pokerReady) < 2 {
				break
			}
			if err := table.deal(time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
}
