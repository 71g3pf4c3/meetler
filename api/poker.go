package main

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
)

const (
	pokerSeats     = 6
	pokerBuyIn     = 1000
	pokerBigBlind  = 10
	pokerTurnLimit = 45 * time.Second
	pokerSeatTTL   = 90 * time.Second
)

type pokerPlayer struct {
	key, name, identity string
	chips, bet, total   int
	cards               []int
	inHand, folded      bool
	acted               bool
	reopenAt            int
	seen                time.Time
	lastAction          string
	lastAmount          int
	net                 int
}

type pokerTable struct {
	players        [pokerSeats]*pokerPlayer
	deck, board    []int
	deadBets       []int
	dealer, turn   int
	bet, minRaise  int
	pot, hand      int
	street, result string
	deadline       time.Time
	reveal         bool
}

type pokerSeatView struct {
	Name       string `json:"name"`
	Chips      int    `json:"chips"`
	Bet        int    `json:"bet"`
	InHand     bool   `json:"inHand"`
	Folded     bool   `json:"folded"`
	You        bool   `json:"you"`
	Cards      []int  `json:"cards,omitempty"`
	CardCount  int    `json:"cardCount"`
	LastAction string `json:"lastAction,omitempty"`
	LastAmount int    `json:"lastAmount,omitempty"`
	Net        *int   `json:"net,omitempty"`
}

type pokerView struct {
	Seats    [pokerSeats]*pokerSeatView `json:"seats"`
	Board    []int                      `json:"board"`
	Dealer   int                        `json:"dealer"`
	Turn     int                        `json:"turn"`
	Bet      int                        `json:"bet"`
	MinRaise int                        `json:"minRaise"`
	Pot      int                        `json:"pot"`
	Hand     int                        `json:"hand"`
	Street   string                     `json:"street"`
	Result   string                     `json:"result"`
	Deadline int64                      `json:"deadline"`
	CanDeal  bool                       `json:"canDeal"`
	CanRaise bool                       `json:"canRaise"`
}

func (t *pokerTable) expire(now time.Time) {
	for i, p := range t.players {
		if p != nil && now.Sub(p.seen) > pokerSeatTTL {
			t.removePlayer(i, now)
		}
	}
	if t.active() && !now.Before(t.deadline) {
		p := t.players[t.turn]
		if p.bet == t.bet {
			_ = t.check(p.key, now)
		} else {
			_ = t.fold(p.key, now)
		}
	}
}

func (t *pokerTable) active() bool { return t.street != "waiting" && t.street != "finished" }

func (t *pokerTable) seat(key string) int {
	for i, p := range t.players {
		if p != nil && p.key == key {
			return i
		}
	}
	return -1
}

func (t *pokerTable) next(after int, eligible func(*pokerPlayer) bool) int {
	for n := 1; n <= pokerSeats; n++ {
		i := (after + n + pokerSeats) % pokerSeats
		if p := t.players[i]; p != nil && eligible(p) {
			return i
		}
	}
	return -1
}

func pokerReady(p *pokerPlayer) bool  { return p.chips > 0 }
func pokerLive(p *pokerPlayer) bool   { return p.inHand && !p.folded }
func pokerCanAct(p *pokerPlayer) bool { return pokerLive(p) && p.chips > 0 }

func (t *pokerTable) count(eligible func(*pokerPlayer) bool) int {
	n := 0
	for _, p := range t.players {
		if p != nil && eligible(p) {
			n++
		}
	}
	return n
}

func (t *pokerTable) pay(p *pokerPlayer, amount int) {
	amount = min(amount, p.chips)
	p.chips -= amount
	p.bet += amount
	p.total += amount
	t.pot += amount
}

func (t *pokerTable) draw() int {
	c := t.deck[0]
	t.deck = t.deck[1:]
	return c
}

func (t *pokerTable) deal(now time.Time) error {
	if t.active() || t.count(pokerReady) < 2 {
		return fmt.Errorf("Для раздачи нужны хотя бы два игрока с фишками")
	}
	deck := make([]int, 52)
	for i := range deck {
		deck[i] = i
	}
	for i := len(deck) - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return fmt.Errorf("Не удалось перемешать карты")
		}
		deck[i], deck[j.Int64()] = deck[j.Int64()], deck[i]
	}
	t.deck, t.board, t.deadBets = deck, nil, nil
	t.pot, t.bet, t.minRaise = 0, pokerBigBlind, pokerBigBlind
	t.street, t.result, t.reveal = "preflop", "", false
	t.hand++
	for _, p := range t.players {
		if p != nil {
			p.inHand, p.folded, p.acted = pokerReady(p), false, false
			p.bet, p.total, p.reopenAt, p.cards = 0, 0, 0, nil
			p.lastAction, p.lastAmount = "", 0
		}
	}
	t.dealer = t.next(t.dealer, pokerReady)
	for range 2 {
		for n := 1; n <= pokerSeats; n++ {
			if p := t.players[(t.dealer+n)%pokerSeats]; p != nil && p.inHand {
				p.cards = append(p.cards, t.draw())
			}
		}
	}
	small := t.next(t.dealer, pokerReady)
	if t.count(pokerReady) == 2 {
		small = t.dealer
	}
	big := t.next(small, pokerReady)
	t.pay(t.players[small], pokerBigBlind/2)
	t.pay(t.players[big], pokerBigBlind)
	t.players[small].lastAction, t.players[small].lastAmount = "smallBlind", t.players[small].bet
	t.players[big].lastAction, t.players[big].lastAmount = "bigBlind", t.players[big].bet
	t.progress(big, now)
	return nil
}

func (t *pokerTable) canRaise(p *pokerPlayer) bool {
	return pokerCanAct(p) && t.count(pokerCanAct) > 1 && p.bet+p.chips > t.bet &&
		(!p.acted || t.bet >= p.reopenAt)
}

func (t *pokerTable) removePlayer(i int, now time.Time) {
	p := t.players[i]
	inHand := t.active() && p.inHand
	if inHand && p.total > 0 {
		t.deadBets = append(t.deadBets, p.total)
	}
	t.players[i] = nil
	if t.players == [pokerSeats]*pokerPlayer{} {
		*t = pokerTable{dealer: -1, turn: -1, street: "waiting", minRaise: pokerBigBlind}
		return
	}
	if inHand {
		after := t.turn - 1
		if t.turn == i {
			after = i
		}
		t.progress(after, now)
	}
}

func (t *pokerTable) sit(key string, seat int, name string, now time.Time) error {
	if t.seat(key) >= 0 {
		return fmt.Errorf("Вы уже за столом")
	}
	if seat < 0 || seat >= pokerSeats || t.players[seat] != nil {
		return fmt.Errorf("Место занято")
	}
	t.players[seat] = &pokerPlayer{key: key, name: sanitizeText(name, 40, "Гость"), chips: pokerBuyIn, seen: now}
	return nil
}

func (t *pokerTable) leave(key string, now time.Time) error {
	if i := t.seat(key); i >= 0 {
		t.removePlayer(i, now)
	}
	return nil
}

func (t *pokerTable) startHand(key string, now time.Time) error {
	if t.seat(key) < 0 {
		return fmt.Errorf("Сначала займите место")
	}
	return t.deal(now)
}

func (t *pokerTable) refill(key string, _ time.Time) error {
	i := t.seat(key)
	if i < 0 {
		return fmt.Errorf("Сначала займите место")
	}
	p := t.players[i]
	if p.chips > 0 || (t.active() && p.inHand) {
		return fmt.Errorf("Пополнить фишки можно после проигрыша стека")
	}
	p.chips = pokerBuyIn
	return nil
}

func (t *pokerTable) actor(key string) (*pokerPlayer, int, error) {
	i := t.seat(key)
	if i < 0 || !t.active() || t.turn != i || !pokerCanAct(t.players[i]) {
		return nil, -1, fmt.Errorf("Сейчас не ваш ход")
	}
	return t.players[i], i, nil
}

func (t *pokerTable) fold(key string, now time.Time) error {
	p, i, err := t.actor(key)
	if err != nil {
		return err
	}
	p.folded = true
	p.lastAction, p.lastAmount = "fold", 0
	t.finishAction(p, i, now)
	return nil
}

func (t *pokerTable) check(key string, now time.Time) error {
	p, i, err := t.actor(key)
	if err != nil {
		return err
	}
	if p.bet != t.bet {
		return fmt.Errorf("Нужно ответить на ставку")
	}
	p.lastAction, p.lastAmount = "check", 0
	t.finishAction(p, i, now)
	return nil
}

func (t *pokerTable) call(key string, now time.Time) error {
	p, i, err := t.actor(key)
	if err != nil {
		return err
	}
	p.lastAction, p.lastAmount = "call", min(t.bet-p.bet, p.chips)
	t.pay(p, t.bet-p.bet)
	t.finishAction(p, i, now)
	return nil
}

func (t *pokerTable) raise(key string, amount int, now time.Time) error {
	p, i, err := t.actor(key)
	if err != nil {
		return err
	}
	maximum := p.bet + p.chips
	if !t.canRaise(p) || amount <= t.bet || amount > maximum ||
		(amount < t.bet+t.minRaise && amount != maximum) {
		return fmt.Errorf("Недопустимое повышение")
	}
	increase := amount - t.bet
	if increase >= t.minRaise {
		t.minRaise = increase
		for _, other := range t.players {
			if other != nil {
				other.acted = false
			}
		}
	}
	p.lastAction, p.lastAmount = "raise", amount
	if t.bet == 0 {
		p.lastAction = "bet"
	}
	t.pay(p, amount-p.bet)
	t.bet = amount
	t.finishAction(p, i, now)
	return nil
}

func (t *pokerTable) finishAction(p *pokerPlayer, i int, now time.Time) {
	p.acted, p.reopenAt = true, t.bet+t.minRaise
	if t.bet == 0 {
		p.reopenAt = 1
	}
	t.progress(i, now)
}

func (t *pokerTable) progress(after int, now time.Time) {
	for {
		if t.count(pokerLive) <= 1 {
			t.finish(false)
			return
		}
		pending := t.next(after, func(p *pokerPlayer) bool {
			return pokerCanAct(p) && (!p.acted || p.bet < t.bet)
		})
		if t.count(pokerCanAct) == 1 {
			i := t.next(-1, pokerCanAct)
			toMatch := 0
			for j, p := range t.players {
				if j != i && p != nil && pokerLive(p) {
					toMatch = max(toMatch, p.bet)
				}
			}
			if t.players[i].bet >= toMatch {
				pending = -1
			}
		}
		if pending >= 0 {
			t.turn, t.deadline = pending, now.Add(pokerTurnLimit)
			return
		}
		if t.street == "river" {
			t.finish(true)
			return
		}
		t.draw()
		n := 1
		switch t.street {
		case "preflop":
			t.street, n = "flop", 3
		case "flop":
			t.street = "turn"
		case "turn":
			t.street = "river"
		}
		for range n {
			t.board = append(t.board, t.draw())
		}
		t.bet, t.minRaise = 0, pokerBigBlind
		for _, p := range t.players {
			if p != nil {
				p.bet, p.acted, p.reopenAt = 0, false, 0
			}
		}
		after = t.dealer
	}
}

func (t *pokerTable) finish(showdown bool) {
	payout := make(map[int]int)
	won := make(map[int]bool)
	if !showdown {
		if i := t.next(t.dealer, pokerLive); i >= 0 {
			payout[i] = t.pot
			won[i] = true
		}
	} else {
		levels := slices.Clone(t.deadBets)
		for _, p := range t.players {
			if p != nil && p.total > 0 {
				levels = append(levels, p.total)
			}
		}
		slices.Sort(levels)
		levels = slices.Compact(levels)
		previous := 0
		for _, level := range levels {
			amount, best := 0, -1
			deadContributors := 0
			for _, contribution := range t.deadBets {
				if contribution >= level {
					amount += level - previous
					deadContributors++
				}
			}
			winners := []int{}
			contributors := []int{}
			for n := 1; n <= pokerSeats; n++ {
				i := (t.dealer + n + pokerSeats) % pokerSeats
				p := t.players[i]
				if p == nil || p.total < level {
					continue
				}
				contributors = append(contributors, i)
				amount += level - previous
				if !pokerLive(p) {
					continue
				}
				value := pokerValue(append(slices.Clone(p.cards), t.board...))
				if value > best {
					best, winners = value, []int{i}
				} else if value == best {
					winners = append(winners, i)
				}
			}
			refund := len(contributors) == 1 && deadContributors == 0
			if refund {
				winners = contributors
			}
			if len(winners) == 0 {
				for n := 1; n <= pokerSeats; n++ {
					i := (t.dealer + n + pokerSeats) % pokerSeats
					if p := t.players[i]; p != nil && pokerLive(p) {
						winners = append(winners, i)
					}
				}
			}
			for n, i := range winners {
				share := amount / len(winners)
				if n < amount%len(winners) {
					share++
				}
				payout[i] += share
				if !refund && share > 0 {
					won[i] = true
				}
			}
			previous = level
		}
	}
	results := []string{}
	for i, p := range t.players {
		if p == nil {
			continue
		}
		p.net = payout[i] - p.total
		p.chips += payout[i]
		if won[i] {
			results = append(results, fmt.Sprintf("%s %+d", p.name, p.net))
		}
	}
	t.street, t.turn, t.reveal = "finished", -1, showdown
	t.deadline = time.Time{}
	t.result = strings.Join(results, "; ")
}

func (t *pokerTable) view(key string) pokerView {
	v := pokerView{
		Board: append([]int{}, t.board...), Dealer: t.dealer, Turn: t.turn,
		Bet: t.bet, MinRaise: t.minRaise, Pot: t.pot, Hand: t.hand,
		Street: t.street, Result: t.result,
		CanDeal: !t.active() && t.count(pokerReady) >= 2 && t.seat(key) >= 0,
	}
	if t.active() {
		v.Deadline = t.deadline.UnixMilli()
	}
	for i, p := range t.players {
		if p == nil {
			continue
		}
		s := &pokerSeatView{
			Name: p.name, Chips: p.chips, Bet: p.bet, InHand: p.inHand,
			Folded: p.folded, You: p.key == key,
			CardCount:  len(p.cards),
			LastAction: p.lastAction, LastAmount: p.lastAmount,
		}
		if t.street == "finished" && p.inHand {
			net := p.net
			s.Net = &net
		}
		if s.You || (t.reveal && pokerLive(p)) {
			s.Cards = slices.Clone(p.cards)
		}
		if s.You && t.turn == i {
			v.CanRaise = t.canRaise(p)
		}
		v.Seats[i] = s
	}
	return v
}

func pokerValue(cards []int) int {
	best := 0
	for a := 0; a < len(cards)-4; a++ {
		for b := a + 1; b < len(cards)-3; b++ {
			for c := b + 1; c < len(cards)-2; c++ {
				for d := c + 1; d < len(cards)-1; d++ {
					for e := d + 1; e < len(cards); e++ {
						best = max(best, pokerFive([5]int{cards[a], cards[b], cards[c], cards[d], cards[e]}))
					}
				}
			}
		}
	}
	return best
}

func pokerFive(cards [5]int) int {
	var counts [15]int
	flush := true
	for _, c := range cards {
		counts[c%13+2]++
		flush = flush && c/13 == cards[0]/13
	}
	ranks := []int{}
	for rank := 14; rank >= 2; rank-- {
		if counts[rank] > 0 {
			ranks = append(ranks, rank)
		}
	}
	straight := 0
	if len(ranks) == 5 {
		if ranks[0]-ranks[4] == 4 {
			straight = ranks[0]
		} else if slices.Equal(ranks, []int{14, 5, 4, 3, 2}) {
			straight = 5
		}
	}
	ordered := slices.Clone(ranks)
	slices.SortFunc(ordered, func(a, b int) int {
		if counts[a] != counts[b] {
			return counts[b] - counts[a]
		}
		return b - a
	})
	category := 0
	switch {
	case straight > 0 && flush:
		category, ordered = 8, []int{straight}
	case counts[ordered[0]] == 4:
		category = 7
	case counts[ordered[0]] == 3 && counts[ordered[1]] == 2:
		category = 6
	case flush:
		category, ordered = 5, ranks
	case straight > 0:
		category, ordered = 4, []int{straight}
	case counts[ordered[0]] == 3:
		category = 3
	case counts[ordered[0]] == 2 && counts[ordered[1]] == 2:
		category = 2
	case counts[ordered[0]] == 2:
		category = 1
	}
	value := category
	for i := range 5 {
		value *= 15
		if i < len(ordered) {
			value += ordered[i]
		}
	}
	return value
}
