export function createPokerTable(onBusted) {
  const $ = (id) => document.getElementById(id);
  const players = $('pokerPlayers');
  const status = $('pokerStatus');
  const actor = $('pokerActor');
  const clock = $('pokerClock');
  const pot = $('pokerPot');
  const communityCards = $('pokerCommunityCards');
  const self = $('pokerSelf');
  const controls = $('pokerSelfControls');
  const selfChips = $('pokerSelfChips');
  const selfAction = $('pokerSelfAction');
  const hand = $('pokerHand');
  const deal = $('pokerDeal');
  const leave = $('pokerLeave');
  const actions = $('pokerActions');
  const fold = $('pokerFold');
  const call = $('pokerCall');
  const raise = $('pokerRaise');
  const bet = $('pokerBet');
  const error = $('pokerErr');
  let path = '', key = '', name = '', identity = '', state = null;
  let timer = null, epoch = 0, revision = 0, pending = false, polling = false;
  let actionRequest = null;
  let betContext = '', canBet = false;

  function element(tag, className, text = '') {
    const el = document.createElement(tag);
    el.className = className;
    el.textContent = text;
    return el;
  }

  function cards(box, values, count = values.length) {
    const suits = ['♠', '♥', '♦', '♣'];
    const names = ['пики', 'червы', 'бубны', 'трефы'];
    const ranks = ['2', '3', '4', '5', '6', '7', '8', '9', '10', 'J', 'Q', 'K', 'A'];
    box.replaceChildren(...Array.from({ length: count }, (_, i) => {
      const value = values[i];
      const card = element('span', 'poker-card');
      if (value === -1) {
        card.classList.add('back');
        card.textContent = '?';
        card.setAttribute('aria-label', 'Закрытая карта');
      } else if (value === undefined) {
        card.classList.add('empty');
        card.setAttribute('aria-label', 'Место для карты');
      } else {
        const suit = Math.floor(value / 13);
        card.classList.toggle('red', suit === 1 || suit === 2);
        card.textContent = ranks[value % 13] + suits[suit];
        card.setAttribute('aria-label', ranks[value % 13] + ' ' + names[suit]);
      }
      return card;
    }));
  }

  const seats = Array.from({ length: 6 }, (_, i) => {
    const el = element('div', 'poker-seat');
    const label = element('div', 'poker-seat-name');
    const chips = element('span', 'poker-stat chips');
    const action = element('span', 'poker-stat');
    const pocket = element('div', 'poker-seat-cards');
    const stats = element('div', 'poker-seat-stats poker-control-column');
    stats.append(chips, action);
    const info = element('div', 'poker-seat-info');
    info.append(stats, pocket);
    const sit = element('button', '', 'Сесть');
    sit.type = 'button';
    sit.setAttribute('aria-label', `Занять место ${i + 1}`);
    sit.onclick = () => act('/seats', { seat: i, name, identity });
    el.append(label, info, sit);
    players.append(el);
    return { el, label, chips, action, pocket, info, sit };
  });

  function showPlayerStatus(el, player) {
    const names = { smallBlind: 'Малый блайнд', bigBlind: 'Большой блайнд', fold: 'Пас', check: 'Чек', call: 'Колл', bet: 'Ставка', raise: 'Повышение' };
    el.hidden = !player;
    if (typeof player?.net === 'number') {
      el.className = `poker-stat ${player.net > 0 ? 'profit' : player.net < 0 ? 'loss' : ''}`;
      el.textContent = `${player.net > 0 ? '+' : ''}${player.net}`;
      el.title = 'Итог раздачи';
      return;
    }
    el.className = `poker-stat ${player?.lastAction || ''}`;
    el.textContent = player?.lastAction ? `${names[player.lastAction]}${player.lastAmount ? ` ${player.lastAmount}` : ''}` : '...';
    el.title = el.textContent;
  }

  function pocketCards(player) {
    return player?.cards?.length ? player.cards : [-1, -1];
  }

  function render() {
    const me = state?.seats.find((p) => p?.you);
    const mySeat = state?.seats.findIndex((p) => p?.you) ?? -1;
    const active = state && !['waiting', 'finished'].includes(state.street);
    if (me?.chips === 0 && (!active || !me.inHand)) {
      onBusted();
      return;
    }
    const myTurn = active && state.turn === mySeat && !pending;
    for (let i = 0; i < seats.length; i++) {
      const ui = seats[i], p = state?.seats[i];
      ui.el.hidden = !!me && (!p || p.you);
      ui.label.textContent = p ? p.name : `Место ${i + 1}`;
      ui.chips.textContent = p ? String(p.chips) : '';
      ui.info.hidden = !p;
      showPlayerStatus(ui.action, p);
      ui.el.classList.toggle('acting', !!active && state.turn === i);
      ui.el.classList.toggle('folded', !!active && !!p?.folded);
      ui.sit.hidden = !!p;
      ui.sit.disabled = !state || !!me || pending;
      cards(ui.pocket, pocketCards(p), 2);
    }
    cards(communityCards, state?.board || [], 5);
    self.hidden = controls.hidden = !me;
    selfChips.textContent = me ? String(me.chips) : '';
    showPlayerStatus(selfAction, me);
    cards(hand, pocketCards(me), 2);
    pot.textContent = String(state?.pot || 0);
    pot.setAttribute('aria-label', `Общий банк: ${state?.pot || 0}`);
    const streets = { waiting: 'Ожидание игроков', preflop: 'Префлоп', flop: 'Флоп', turn: 'Тёрн', river: 'Ривер', finished: state?.result };
    status.textContent = state ? streets[state.street] : 'Ожидание игроков';
    actor.hidden = clock.hidden = !active;
    const acting = active ? state.seats[state.turn] : null;
    actor.textContent = acting?.name || '';
    clock.textContent = '';
    if (active) {
      const seconds = Math.max(0, Math.ceil((state.deadline - Date.now()) / 1000));
      clock.textContent = `${seconds}с`;
    }
    deal.hidden = !me || !!active;
    deal.disabled = !state?.canDeal || pending;
    leave.hidden = !me;
    leave.disabled = pending;
    actions.hidden = !active || !me?.inHand || me.folded || !me.chips;
    fold.disabled = call.disabled = !myTurn;
    const toCall = me && state ? Math.min(me.chips, Math.max(0, state.bet - me.bet)) : 0;
    call.textContent = toCall ? `Колл ${toCall}` : 'Чек';
    canBet = !!myTurn && !!state?.canRaise;
    raise.textContent = state?.bet ? 'Повысить' : 'Поставить';
    if (me && state) {
      const maximum = me.bet + me.chips;
      const minimum = Math.min(maximum, state.bet + state.minRaise);
      bet.min = String(minimum);
      bet.max = String(maximum);
      bet.placeholder = String(minimum);
      const context = `${state.hand}:${state.street}:${state.bet}:${state.minRaise}:${maximum}`;
      if (context !== betContext) {
        bet.value = String(minimum);
        betContext = context;
      }
    }
    updateBet();
  }

  async function request(endpoint = '', method = 'GET', body) {
    const currentEpoch = epoch;
    const currentRevision = revision;
    const response = await fetch(path + endpoint, {
      method,
      headers: { 'X-Poker-Player': key, ...(body ? { 'Content-Type': 'application/json' } : {}) },
      ...(body ? { body: JSON.stringify(body) } : {}),
      cache: 'no-store',
    });
    if (!response.ok) throw new Error(await response.text());
    if (method !== 'GET') return;
    const next = await response.json();
    if (currentEpoch === epoch && currentRevision === revision) {
      state = next;
      error.textContent = '';
      render();
    }
  }

  async function refresh() {
    if (!path || pending || polling) return;
    const currentEpoch = epoch;
    polling = true;
    try {
      await request();
    } catch (e) {
      if (currentEpoch === epoch) error.textContent = e.message || 'Не удалось обновить стол';
    } finally {
      if (currentEpoch === epoch) polling = false;
    }
  }

  async function act(endpoint, body, method = 'POST') {
    if (!path || pending) return;
    const currentEpoch = epoch;
    revision++;
    pending = true;
    render();
    try {
      actionRequest = request(endpoint, method, body);
      await actionRequest;
      if (currentEpoch === epoch) await request();
    } catch (e) {
      if (currentEpoch === epoch) error.textContent = e.message || 'Не удалось выполнить действие';
    } finally {
      if (currentEpoch === epoch) {
        pending = false;
        actionRequest = null;
        render();
      }
    }
  }

  deal.onclick = () => act('/deal');
  leave.onclick = () => act('/seats/me', null, 'DELETE');
  fold.onclick = () => act('/fold');
  call.onclick = () => {
    const me = state?.seats.find((p) => p?.you);
    return act(me && state.bet > me.bet ? '/call' : '/check');
  };
  actions.onsubmit = (event) => {
    event.preventDefault();
    updateBet();
    if (raise.disabled) return;
    return act('/raise', { amount: Number(bet.value) });
  };
  function updateBet() {
    if (Number(bet.value) > Number(bet.max)) bet.value = bet.max;
    const amount = Number(bet.value);
    raise.disabled = !canBet || !bet.value || !Number.isInteger(amount) || amount < Number(bet.min);
  }
  bet.oninput = updateBet;

  function disconnect() {
    const oldPath = path, oldKey = key, finishing = actionRequest;
    epoch++;
    clearInterval(timer);
    path = '';
    state = null;
    pending = polling = false;
    actionRequest = null;
    betContext = '';
    error.textContent = '';
    render();
    if (oldPath) {
      const sendLeave = () => fetch(oldPath + '/seats/me', {
        method: 'DELETE', keepalive: true,
        headers: { 'X-Poker-Player': oldKey },
      }).catch(() => {});
      sendLeave();
      if (finishing) Promise.resolve(finishing).catch(() => {}).then(sendLeave);
    }
  }

  render();
  return {
    connect(room, playerName, participantIdentity) {
      disconnect();
      key = sessionStorage.getItem('meetlerPokerKey');
      if (!key) {
        key = crypto.randomUUID();
        sessionStorage.setItem('meetlerPokerKey', key);
      }
      name = playerName;
      identity = participantIdentity;
      path = '/api/rooms/' + encodeURIComponent(room) + '/poker';
      refresh();
      timer = setInterval(refresh, 1000);
    },
    disconnect,
  };
}
