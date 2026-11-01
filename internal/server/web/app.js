'use strict';

(function () {
  const $ = (sel) => document.querySelector(sel);

  const state = {
    ws: null,
    devices: [],
    ice: [],
    file: null,
    loggedIn: false,
    everOpened: false,
    dormant: false,
    lastActivity: Date.now(),
    mode: 'login',
    pending: new Map(),
    sending: false,
    receive: false,
    recvID: '',
    recvName: '',
    recvPlatform: '',
    recvDirHandle: null,
    recvPeers: new Map(),
    recvCandidates: new Map(),
    recvOffers: new Map(),
    recvNames: new Set(),
    recvItems: [],
  };

  const RECV_LIST_MAX = 12;
  const OFFER_TTL = 5 * 60 * 1000;
  const ANSWER_WAIT = 5 * 60 * 1000;
  const IDLE_MS = 30 * 60 * 1000;
  const IDLE_TICK = 30 * 1000;

  document.addEventListener('DOMContentLoaded', init);

  function init() {
    bindAuth();
    bindMain();
    bindIdleWatch();
    ensureRecvIdentity();
    fetch('/api/ice')
      .then((r) => (r.ok ? r.json() : null))
      .then((d) => {
        if (d && Array.isArray(d.urls)) state.ice = d.urls;
      })
      .catch(() => {});
    fetch('/api/me')
      .then((r) => (r.ok ? r.json() : null))
      .then((me) => {
        if (me && me.username) showApp(me.username);
      })
      .catch(() => {});
  }

  function bindAuth() {
    $('#tabLogin').addEventListener('click', () => setAuthMode('login'));
    $('#tabRegister').addEventListener('click', () => setAuthMode('register'));
    $('#authForm').addEventListener('submit', async (ev) => {
      ev.preventDefault();
      const username = $('#username').value.trim();
      const password = $('#password').value;
      const submit = $('#authSubmit');
      $('#authError').textContent = '';
      submit.disabled = true;
      try {
        if (state.mode === 'register') {
          await apiPost('/api/register', { username, password });
        }
        const me = await apiPost('/api/login', { username, password });
        showApp(me.username);
      } catch (err) {
        $('#authError').textContent = err && err.message ? err.message : '请求失败';
      } finally {
        submit.disabled = false;
      }
    });
  }

  function setAuthMode(mode) {
    state.mode = mode;
    const isLogin = mode === 'login';
    $('#tabLogin').classList.toggle('active', isLogin);
    $('#tabRegister').classList.toggle('active', !isLogin);
    $('#authSubmit').textContent = isLogin ? '登录' : '注册并登录';
    $('#password').setAttribute('autocomplete', isLogin ? 'current-password' : 'new-password');
    $('#authError').textContent = '';
  }

  async function apiPost(path, body) {
    const resp = await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    let data = null;
    try {
      data = await resp.json();
    } catch (err) {
      data = null;
    }
    if (!resp.ok) {
      throw new Error((data && data.error) || '请求失败（HTTP ' + resp.status + '）');
    }
    return data || {};
  }

  function showApp(username) {
    if (!username) return;
    state.loggedIn = true;
    state.dormant = false;
    state.lastActivity = Date.now();
    $('#auth').hidden = true;
    $('#app').hidden = false;
    $('#whoami').textContent = username;
    ensureRecvIdentity();
    const label = browserLabel();
    state.recvName = label + ' 页面';
    state.recvPlatform = 'web/' + label.toLowerCase();
    state.receive = true;
    renderReceivePanel();
    connectWS();
    loadDevices();
  }

  function showAuth() {
    state.loggedIn = false;
    state.everOpened = false;
    state.dormant = false;
    state.receive = false;
    clearOffers();
    state.recvDirHandle = null;
    state.recvNames.clear();
    state.recvItems = [];
    for (const from of Array.from(state.recvPeers.keys())) closeRecvPeer(from);
    state.recvCandidates.clear();
    $('#recvPanel').hidden = true;
    $('#recvList').textContent = '';
    if (state.ws) {
      try {
        state.ws.close();
      } catch (err) {
        /* ignore */
      }
      state.ws = null;
    }
    state.devices = [];
    setConnStatus('off', '');
    $('#app').hidden = true;
    $('#auth').hidden = false;
    $('#password').value = '';
  }

  function wsOnline() {
    return !!state.ws && state.ws.readyState === WebSocket.OPEN;
  }

  function setConnStatus(kind, text) {
    const el = $('#connStatus');
    if (!el) return;
    el.textContent = text;
    el.hidden = !text;
    el.className = 'conn' + (text ? ' ' + kind : '');
    el.title = kind === 'sleep' ? '页面已休眠，操作一下（点击/按键）即可唤醒' : '';
  }

  function bindIdleWatch() {
    const mark = () => {
      state.lastActivity = Date.now();
      if (state.dormant) wakeUp();
    };
    ['pointerdown', 'keydown', 'wheel', 'touchstart'].forEach((type) =>
      window.addEventListener(type, mark, { passive: true, capture: true })
    );
    window.addEventListener('focus', mark);
    document.addEventListener('visibilitychange', () => {
      if (!document.hidden) mark();
    });
    setInterval(() => {
      if (!state.loggedIn || state.dormant) return;
      if (Date.now() - state.lastActivity < IDLE_MS) return;
      if (isBusy()) {
        state.lastActivity = Date.now();
        return;
      }
      goDormant();
    }, IDLE_TICK);
  }

  function isBusy() {
    return (
      state.sending ||
      state.pending.size > 0 ||
      state.recvPeers.size > 0 ||
      state.recvOffers.size > 0
    );
  }

  function goDormant() {
    state.dormant = true;
    const ws = state.ws;
    state.ws = null;
    if (ws) {
      ws.onclose = null;
      try {
        ws.close();
      } catch (err) {
        /* ignore */
      }
    }
    clearOffers();
    setConnStatus('sleep', '已休眠');
    renderDevices();
  }

  function wakeUp() {
    if (!state.dormant) return;
    state.dormant = false;
    state.lastActivity = Date.now();
    if (!state.loggedIn) return;
    connectWS();
  }

  function connectWS() {
    if (!state.loggedIn || state.dormant) return;
    if (state.ws && state.ws.readyState <= WebSocket.OPEN) return;
    const scheme = location.protocol === 'https:' ? 'wss://' : 'ws://';
    let url = scheme + location.host + '/api/ws';
    if (state.receive) {
      const params = new URLSearchParams({
        device_id: state.recvID,
        name: state.recvName,
        platform: state.recvPlatform,
        ephemeral: '1',
      });
      url += '?' + params.toString();
    }
    const ws = new WebSocket(url);
    state.ws = ws;
    setConnStatus('mid', state.everOpened ? '重连中' : '连接中');
    ws.onopen = () => {
      state.everOpened = true;
      setConnStatus('on', '本端在线');
      renderDevices();
      reportPublicAddr(ws);
    };
    ws.onmessage = (ev) => {
      let msg;
      try {
        msg = JSON.parse(ev.data);
      } catch (err) {
        return;
      }
      if (msg.type === 'hello') {
        if (msg.devices) {
          state.devices = msg.devices;
          renderDevices();
        }
      } else if (msg.type === 'devices') {
        state.devices = msg.devices || [];
        renderDevices();
      } else if (msg.type === 'signal') {
        const data = msg.data;
        if (data && data.kind === 'offer') {
          handleIncomingSignal(msg.from, data);
          return;
        }
        const handler = state.pending.get(msg.from);
        if (handler) handler(data);
        if (data && data.kind === 'candidate') handleIncomingSignal(msg.from, data);
      } else if (msg.type === 'error') {
        console.warn('服务端提示:', msg.error);
      }
    };
    ws.onclose = async () => {
      if (state.ws === ws) state.ws = null;
      clearOffers();
      if (!state.loggedIn || state.dormant) return;
      setConnStatus('off', '连接已断开');
      renderDevices();
      await sleep(2000);
      setConnStatus('mid', '重连中');
      if (!state.loggedIn || state.dormant) return;
      try {
        const resp = await fetch('/api/me');
        if (!resp.ok) {
          showAuth();
          return;
        }
      } catch (err) {
        /* 网络暂时不可用，继续重连 */
      }
      connectWS();
    };
  }

  async function reportPublicAddr(ws) {
    const deadline = Date.now() + 5000;
    while (!state.ice.length && Date.now() < deadline) await sleep(200);
    if (!state.ice.length) return;
    const addr = await probePublicAddr();
    if (!addr || ws.readyState !== WebSocket.OPEN) return;
    try {
      ws.send(JSON.stringify({ type: 'info', public_addr: addr }));
    } catch (err) {
      /* ignore */
    }
  }

  function probePublicAddr() {
    if (typeof RTCPeerConnection !== 'function') return Promise.resolve('');
    return new Promise((resolve) => {
      let pc = null;
      let timer = null;
      const finish = (value) => {
        if (timer) clearTimeout(timer);
        if (pc) {
          try {
            pc.close();
          } catch (err) {
            /* ignore */
          }
        }
        resolve(value);
      };
      timer = setTimeout(() => finish(''), 4000);
      try {
        pc = new RTCPeerConnection({ iceServers: [{ urls: state.ice.slice() }] });
      } catch (err) {
        finish('');
        return;
      }
      pc.createDataChannel('probe');
      pc.onicecandidate = (ev) => {
        const cand = ev.candidate;
        if (!cand) return;
        const parts = String(cand.candidate || '').split(' ');
        const type = cand.type || candidateType(parts);
        if (type !== 'srflx') return;
        const ip = cand.address || parts[4];
        const port = cand.port || Number(parts[5]);
        if (!ip || !port) return;
        finish(formatAddr(ip, port));
      };
      pc.createOffer()
        .then((offer) => pc.setLocalDescription(offer))
        .catch(() => finish(''));
    });
  }

  function candidateType(parts) {
    const idx = parts.indexOf('typ');
    return idx >= 0 ? parts[idx + 1] : '';
  }

  function formatAddr(ip, port) {
    return (ip.indexOf(':') >= 0 ? '[' + ip + ']' : ip) + ':' + port;
  }

  async function loadDevices() {
    try {
      const resp = await fetch('/api/devices');
      if (resp.status === 401) {
        showAuth();
        return;
      }
      if (!resp.ok) return;
      const devices = await resp.json();
      if (Array.isArray(devices)) {
        state.devices = devices;
        renderDevices();
      }
    } catch (err) {
      /* ignore */
    }
  }

  function renderDevices() {
    const list = $('#devices');
    const checked = new Set(
      Array.from(list.querySelectorAll('input[type=checkbox]:checked')).map((el) => el.dataset.id)
    );
    list.textContent = '';

    if (!state.devices.length) {
      const li = document.createElement('li');
      li.className = 'empty';
      li.textContent = '还没有设备。在目标设备上运行 filet agent 后会自动出现在这里。';
      list.appendChild(li);
      updateSendState();
      return;
    }

    for (const dev of state.devices) {
      const own = state.receive && dev.id === state.recvID;
      const online = own ? wsOnline() : dev.online;
      const li = document.createElement('li');
      li.className = 'device' + (online ? '' : ' offline');

      const label = document.createElement('label');
      const cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.disabled = !dev.online || own;
      cb.dataset.id = dev.id;
      cb.checked = dev.online && !own && checked.has(dev.id);
      cb.addEventListener('change', updateSendState);

      const main = document.createElement('div');
      main.className = 'd-main';
      const name = document.createElement('span');
      name.className = 'd-name';
      name.textContent = dev.name;
      const meta = document.createElement('span');
      meta.className = 'd-meta';
      const bits = [dev.platform || 'unknown', shortID(dev.id)];
      if (dev.last_seen) bits.push(fmtTime(dev.last_seen));
      meta.textContent = bits.join(' · ');
      main.append(name, meta);

      if (dev.public_addr || dev.location) {
        const net = document.createElement('span');
        net.className = 'd-net';
        net.textContent = [dev.public_addr ? '外网 ' + dev.public_addr : '', dev.location]
          .filter(Boolean)
          .join(' · ');
        main.append(net);
      }

      const badge = document.createElement('span');
      badge.className = 'badge ' + (own ? (online ? 'web' : 'off') : online ? 'on' : 'off');
      badge.textContent = own
        ? online
          ? '本页面 · 在线'
          : '本页面 · 离线'
        : online
          ? '在线'
          : '离线';

      label.append(cb, main, badge);
      li.appendChild(label);
      list.appendChild(li);
    }
    updateSendState();
  }

  function bindMain() {
    $('#logout').addEventListener('click', async () => {
      try {
        await fetch('/api/logout', { method: 'POST' });
      } catch (err) {
        /* ignore */
      }
      showAuth();
    });
    $('#pickDir').addEventListener('click', async () => {
      if (typeof window.showDirectoryPicker !== 'function') return;
      try {
        state.recvDirHandle = await window.showDirectoryPicker({ mode: 'readwrite' });
        state.recvNames.clear();
      } catch (err) {
        return;
      }
      renderReceivePanel();
    });
    $('#refresh').addEventListener('click', loadDevices);
    $('#pick').addEventListener('click', () => $('#fileInput').click());
    $('#fileInput').addEventListener('change', (ev) => {
      const file = ev.target.files && ev.target.files[0];
      if (file) setFile(file);
    });
    $('#selectOnline').addEventListener('click', () => {
      document
        .querySelectorAll('#devices input[type=checkbox]:not(:disabled)')
        .forEach((cb) => {
          cb.checked = true;
        });
      updateSendState();
    });
    $('#send').addEventListener('click', startSend);

    const drop = $('#drop');
    drop.addEventListener('dragover', (ev) => {
      ev.preventDefault();
      drop.classList.add('over');
    });
    drop.addEventListener('dragleave', () => drop.classList.remove('over'));
    drop.addEventListener('drop', (ev) => {
      ev.preventDefault();
      drop.classList.remove('over');
      const file = ev.dataTransfer.files && ev.dataTransfer.files[0];
      if (file) setFile(file);
    });
  }

  function setFile(file) {
    state.file = file;
    $('#fileLabel').textContent = file.name + '（' + humanBytes(file.size) + '）';
    updateSendState();
  }

  function updateSendState() {
    const count = document.querySelectorAll('#devices input[type=checkbox]:checked').length;
    const btn = $('#send');
    btn.disabled = !state.file || count === 0 || state.sending;
    if (state.sending) {
      btn.textContent = '发送中…';
    } else {
      btn.textContent = count ? '发送给选中设备（' + count + '）' : '发送给选中设备';
    }
  }

  async function startSend() {
    if (state.sending || !state.file) return;
    const selected = Array.from(document.querySelectorAll('#devices input[type=checkbox]:checked'))
      .map((cb) => state.devices.find((d) => d.id === cb.dataset.id))
      .filter(Boolean);
    if (!selected.length) return;

    state.sending = true;
    updateSendState();
    $('#transfers').textContent = '';

    let failed = 0;
    for (const dev of selected) {
      const ui = createTransferRow(dev, state.file);
      ui.connecting('连接中…');
      try {
        const result = await sendToDevice(dev, state.file, ui);
        ui.done(result.path || '');
      } catch (err) {
        ui.fail(err && err.message ? err.message : String(err));
        failed++;
      }
    }

    state.sending = false;
    updateSendState();
    if (failed > 0) console.warn(failed + ' 台设备发送失败');
  }

  async function sendToDevice(dev, file, ui) {
    const iceServers = state.ice.length ? [{ urls: state.ice.slice() }] : [];
    const pc = new RTCPeerConnection({ iceServers });
    let remoteSet = false;
    const queued = [];
    let answerResolve = null;
    let answerReject = null;
    const answerPromise = new Promise((resolve, reject) => {
      answerResolve = resolve;
      answerReject = reject;
    });

    state.pending.set(dev.id, (data) => {
      if (!data) return;
      if (data.kind === 'answer' && data.description) {
        answerResolve(data.description);
      } else if (data.kind === 'reject') {
        answerReject(new Error('对方拒绝接收文件（或接收请求已过期）'));
      } else if (data.kind === 'candidate' && data.candidate) {
        if (remoteSet) {
          pc.addIceCandidate(data.candidate).catch(() => {});
        } else {
          queued.push(data.candidate);
        }
      }
    });

    try {
      const dc = pc.createDataChannel('file');
      pc.onicecandidate = (ev) => {
        if (ev.candidate) {
          signalSend(dev.id, { kind: 'candidate', candidate: ev.candidate.toJSON() }).catch(() => {});
        }
      };

      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      ui.connecting('等待对方接收…');
      await signalSend(dev.id, {
        kind: 'offer',
        name: file.name,
        size: file.size,
        description: { type: pc.localDescription.type, sdp: pc.localDescription.sdp },
      });

      const answer = await withTimeout(
        answerPromise,
        ANSWER_WAIT,
        '等待对方接收超时（5 分钟）：设备可能不在线，或对方未点击接收'
      );
      await pc.setRemoteDescription(answer);
      remoteSet = true;
      for (const cand of queued) {
        try {
          await pc.addIceCandidate(cand);
        } catch (err) {
          /* ignore */
        }
      }

      await waitChannelOpen(dc, 30000);
      ui.connecting('传输中…');
      return await transferOverChannel(dc, file, ui);
    } finally {
      state.pending.delete(dev.id);
      try {
        pc.close();
      } catch (err) {
        /* ignore */
      }
    }
  }

  function signalSend(to, data) {
    return new Promise((resolve, reject) => {
      const ws = state.ws;
      if (!ws || ws.readyState !== WebSocket.OPEN) {
        reject(new Error('信令连接已断开，请刷新页面后重试'));
        return;
      }
      ws.send(JSON.stringify({ type: 'signal', to: to, data: data }));
      resolve();
    });
  }

  function waitChannelOpen(dc, ms) {
    if (dc.readyState === 'open') return Promise.resolve();
    if (dc.readyState === 'closing' || dc.readyState === 'closed') {
      return Promise.reject(new Error('数据通道已关闭'));
    }
    return withTimeout(
      new Promise((resolve, reject) => {
        dc.onopen = () => resolve();
        dc.onclose = () => reject(new Error('直连建立超时：双方可能都在对称 NAT / CGNAT 后'));
      }),
      ms,
      '直连建立超时：双方可能都在对称 NAT / CGNAT 后，或 UDP 被防火墙拦截'
    );
  }

  async function transferOverChannel(dc, file, ui) {
    if (dc.readyState !== 'open') throw new Error('数据通道已关闭');

    const CHUNK = 16 * 1024;
    const HIGH = 1024 * 1024;

    const controls = [];
    const waiters = [];
    let doneResolve = null;
    let doneReject = null;
    const donePromise = new Promise((resolve, reject) => {
      doneResolve = resolve;
      doneReject = reject;
    });

    dc.onmessage = (ev) => {
      if (typeof ev.data !== 'string') return;
      let ctrl;
      try {
        ctrl = JSON.parse(ev.data);
      } catch (err) {
        return;
      }
      if (ctrl.kind === 'done') {
        try {
          dc.send(JSON.stringify({ kind: 'ack' }));
        } catch (err) {
          /* 对端可能已经关通道，回执只是礼貌延后它的关闭 */
        }
        doneResolve(ctrl);
        return;
      }
      const idx = waiters.findIndex((w) => w.kind === ctrl.kind);
      if (idx >= 0) {
        const waiter = waiters.splice(idx, 1)[0];
        waiter.resolve(ctrl);
      } else {
        controls.push(ctrl);
      }
    };
    dc.onclose = () => doneReject(new Error('数据通道在传输中关闭'));

    function waitControl(kind, ms, message) {
      const idx = controls.findIndex((c) => c.kind === kind);
      if (idx >= 0) return Promise.resolve(controls.splice(idx, 1)[0]);
      return withTimeout(
        new Promise((resolve) => waiters.push({ kind: kind, resolve: resolve })),
        ms,
        message
      );
    }

    dc.send(JSON.stringify({ kind: 'meta', name: file.name, size: file.size }));

    const first = await Promise.race([
      waitControl('ready', 30000, '接收方未就绪'),
      donePromise.then((ctrl) => {
        if (!ctrl.ok) throw new Error(ctrl.error || '接收方拒绝接收');
        return ctrl;
      }),
    ]);
    if (first && first.kind === 'done' && first.ok) return first;

    const start = performance.now();
    let sent = 0;
    for (let offset = 0; offset < file.size; offset += CHUNK) {
      if (dc.readyState !== 'open') throw new Error('数据通道已关闭');
      while (dc.bufferedAmount > HIGH) {
        if (dc.readyState !== 'open') throw new Error('数据通道已关闭');
        await sleep(20);
      }
      const buf = await file.slice(offset, offset + CHUNK).arrayBuffer();
      dc.send(buf);
      sent += buf.byteLength;
      ui.progress(sent, file.size, start);
    }

    const done = await withTimeout(donePromise, 10 * 60 * 1000, '等待接收确认超时');
    if (!done.ok) throw new Error(done.error || '接收失败');
    return done;
  }

  function ensureRecvIdentity() {
    let id = sessionStorage.getItem('filet.recv.id');
    if (!id || !/^[A-Za-z0-9_-]{8,64}$/.test(id)) {
      id = 'web-' + randomHex(16);
      sessionStorage.setItem('filet.recv.id', id);
    }
    state.recvID = id;
  }

  function browserLabel() {
    const ua = navigator.userAgent;
    if (/Edg\//.test(ua)) return 'Edge';
    if (/OPR\//.test(ua)) return 'Opera';
    if (/Firefox\//.test(ua)) return 'Firefox';
    if (/Chrome\//.test(ua)) return 'Chrome';
    if (/Safari\//.test(ua)) return 'Safari';
    return '浏览器';
  }

  function randomHex(n) {
    const bytes = new Uint8Array(n);
    crypto.getRandomValues(bytes);
    return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
  }

  function renderReceivePanel() {
    const panel = $('#recvPanel');
    if (!state.loggedIn) {
      panel.hidden = true;
      return;
    }
    panel.hidden = false;
    const canPick = typeof window.showDirectoryPicker === 'function';
    const pick = $('#pickDir');
    pick.hidden = !canPick;
    pick.textContent = state.recvDirHandle ? '更换目录' : '选择保存目录';
    let text =
      '本页面默认既能发送也能接收（' +
      state.recvName +
      '）。别人发文件给你时，会先出现在下方列表，点击「接收」后文件才开始传输。';
    text +=
      ' 收到的文件保存到 ' +
      (state.recvDirHandle ? '「' + state.recvDirHandle.name + '」' : '浏览器默认下载目录') +
      '，点击条目即可下载（最多保留 ' + RECV_LIST_MAX + ' 条）。';
    if (!state.recvDirHandle) {
      text += canPick
        ? ' 未选择目录时文件会先缓存在内存，大文件建议先选目录。'
        : ' 当前浏览器不支持目录直写，文件会先缓存在内存，超大文件可能失败。';
    }
    $('#recvStatus').textContent = text;
  }

  function clearOffers() {
    for (const from of Array.from(state.recvOffers.keys())) dropOffer(from);
  }

  function dropOffer(from) {
    const offer = state.recvOffers.get(from);
    if (!offer) return;
    state.recvOffers.delete(from);
    if (offer.timer) clearTimeout(offer.timer);
    if (offer.row && offer.row.li.parentNode) offer.row.li.parentNode.removeChild(offer.row.li);
    state.recvCandidates.delete(from);
  }

  function deviceName(id) {
    const dev = state.devices.find((d) => d.id === id);
    if (dev && dev.name) return dev.name;
    if (id && id.indexOf('p-') === 0) return '命令行客户端';
    return shortID(id);
  }

  function handleIncomingSignal(from, data) {
    if (!data) return;
    if (data.kind === 'offer' && data.description) {
      if (state.recvOffers.has(from)) dropOffer(from);
      const row = createPendingRow(
        deviceName(from),
        data.name || '文件',
        Number(data.size) || 0,
        () => acceptOffer(from),
        () => rejectOffer(from)
      );
      const offer = { description: data.description, row: row, timer: null };
      offer.timer = setTimeout(() => {
        if (state.recvOffers.get(from) !== offer) return;
        state.recvOffers.delete(from);
        state.recvCandidates.delete(from);
        signalSend(from, { kind: 'reject' }).catch(() => {});
        row.expired();
      }, OFFER_TTL);
      state.recvOffers.set(from, offer);
      return;
    }
    if (data.kind === 'candidate' && data.candidate) {
      addRecvCandidate(from, data.candidate);
    }
  }

  async function acceptOffer(from) {
    const offer = state.recvOffers.get(from);
    if (!offer) return;
    state.recvOffers.delete(from);
    if (offer.timer) clearTimeout(offer.timer);
    offer.row.accepting();
    try {
      await handleOffer(from, offer.description, offer.row);
      setTimeout(() => {
        const entry = state.recvPeers.get(from);
        if (entry && !entry.opened) {
          offer.row.fail('直连建立超时：双方可能都在对称 NAT / CGNAT 后');
          closeRecvPeer(from);
        }
      }, 35000);
    } catch (err) {
      console.warn('处理来电失败:', err);
      offer.row.fail('建立直连失败：' + (err && err.message ? err.message : err));
      signalSend(from, { kind: 'reject' }).catch(() => {});
      closeRecvPeer(from);
      state.recvCandidates.delete(from);
    }
  }

  function rejectOffer(from) {
    const offer = state.recvOffers.get(from);
    if (!offer) return;
    state.recvOffers.delete(from);
    if (offer.timer) clearTimeout(offer.timer);
    state.recvCandidates.delete(from);
    signalSend(from, { kind: 'reject' }).catch(() => {});
    offer.row.rejected();
  }

  async function handleOffer(from, description, ui) {
    closeRecvPeer(from);
    const iceServers = state.ice.length ? [{ urls: state.ice.slice() }] : [];
    const pc = new RTCPeerConnection({ iceServers });
    const entry = { pc: pc, queued: [], remoteSet: false };
    state.recvPeers.set(from, entry);

    pc.onicecandidate = (ev) => {
      if (ev.candidate) {
        signalSend(from, { kind: 'candidate', candidate: ev.candidate.toJSON() }).catch(() => {});
      }
    };
    pc.ondatachannel = (ev) => {
      entry.opened = true;
      receiveDataChannel(ev.channel, from, ui);
    };
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
        closeRecvPeer(from);
      }
    };

    await pc.setRemoteDescription({ type: description.type, sdp: description.sdp });
    entry.remoteSet = true;
    const pending = state.recvCandidates.get(from);
    if (pending) {
      entry.queued.push(...pending);
      state.recvCandidates.delete(from);
    }
    for (const cand of entry.queued.splice(0)) {
      try {
        await pc.addIceCandidate(cand);
      } catch (err) {
        /* ignore */
      }
    }
    const answer = await pc.createAnswer();
    await pc.setLocalDescription(answer);
    await signalSend(from, {
      kind: 'answer',
      description: { type: pc.localDescription.type, sdp: pc.localDescription.sdp },
    });
  }

  function addRecvCandidate(from, candidate) {
    const entry = state.recvPeers.get(from);
    if (!entry) {
      const list = state.recvCandidates.get(from) || [];
      list.push(candidate);
      state.recvCandidates.set(from, list);
      if (!state.recvOffers.has(from)) {
        setTimeout(() => {
          if (!state.recvPeers.has(from) && !state.recvOffers.has(from)) {
            state.recvCandidates.delete(from);
          }
        }, 60 * 1000);
      }
      return;
    }
    if (entry.remoteSet) {
      entry.pc.addIceCandidate(candidate).catch(() => {});
    } else {
      entry.queued.push(candidate);
    }
  }

  function closeRecvPeer(from) {
    const entry = state.recvPeers.get(from);
    if (!entry) return;
    state.recvPeers.delete(from);
    try {
      entry.pc.close();
    } catch (err) {
      /* ignore */
    }
  }

  async function receiveDataChannel(dc, from, ui) {
    ui = ui || createReceiveRow();
    let meta = null;
    let sink = null;
    let writeChain = Promise.resolve();
    let received = 0;
    let startedAt = 0;
    let finished = false;

    const abortSink = async () => {
      if (!sink || !sink.abort) return;
      try {
        await sink.abort();
      } catch (err) {
        /* ignore */
      }
    };

    // done 发出去就立刻关通道，会把还在链路里的 done 一起掐掉，
    // 发送端只能报「数据通道在传输中关闭」。等它回执，最多 5 秒。
    let resolveAck = null;
    const ackArrived = new Promise((res) => {
      resolveAck = res;
    });
    const releasePeer = () => {
      Promise.race([ackArrived, sleep(5000)]).then(() => closeRecvPeer(from));
    };

    const succeed = async () => {
      if (finished) return;
      finished = true;
      let item = null;
      try {
        await writeChain;
        if (sink.kind === 'file') {
          await sink.close();
          item = {
            name: sink.name,
            handle: sink.fileHandle,
            hint: '已保存到 ' + (state.recvDirHandle ? state.recvDirHandle.name + '/' : '') + sink.name,
          };
        } else {
          item = {
            name: sink.name,
            blob: new Blob(sink.parts, { type: 'application/octet-stream' }),
            hint: humanBytes(meta.size) + ' · 点击下载',
          };
        }
        addRecvEntry(ui, item);
        ui.done(item.hint);
      } catch (err) {
        const text = '写入文件失败：' + (err && err.message ? err.message : err);
        ui.fail(text);
        await abortSink();
        try {
          dc.send(JSON.stringify({ kind: 'done', ok: false, error: text }));
        } catch (e) {
          /* ignore */
        }
        releasePeer();
        return;
      }
      try {
        dc.send(JSON.stringify({ kind: 'done', ok: true, path: sink.name || '' }));
      } catch (err) {
        /* ignore */
      }
      releasePeer();
    };

    const fail = async (message) => {
      if (finished) return;
      finished = true;
      ui.fail(message);
      await abortSink();
      try {
        dc.send(JSON.stringify({ kind: 'done', ok: false, error: message }));
      } catch (err) {
        /* ignore */
      }
      releasePeer();
    };

    dc.binaryType = 'arraybuffer';
    dc.onmessage = async (ev) => {
      if (typeof ev.data === 'string' && ev.data.charAt(0) === '{') {
        let kind = '';
        try {
          kind = (JSON.parse(ev.data) || {}).kind || '';
        } catch (err) {
          /* 非控制帧 */
        }
        if (kind === 'ack') {
          resolveAck();
          return;
        }
      }
      if (finished) return;
      if (typeof ev.data === 'string') {
        let ctrl;
        try {
          ctrl = JSON.parse(ev.data);
        } catch (err) {
          return;
        }
        if (ctrl.kind !== 'meta' || meta) return;
        meta = ctrl;
        const total = Number(ctrl.size);
        if (!Number.isFinite(total) || total < 0) {
          await fail('无法识别文件大小');
          return;
        }
        const name = sanitizeName(meta.name);
        ui.meta(name, total);
        try {
          sink = await openRecvSink(name);
        } catch (err) {
          await fail('无法创建保存文件：' + (err && err.message ? err.message : err));
          return;
        }
        startedAt = performance.now();
        try {
          dc.send(JSON.stringify({ kind: 'ready' }));
        } catch (err) {
          /* ignore */
        }
        if (total === 0) await succeed();
        return;
      }
      if (!meta || !sink) return;
      const buf = ev.data;
      received += buf.byteLength;
      if (received > meta.size) {
        await fail('接收数据超过声明大小');
        return;
      }
      writeChain = writeChain.then(() => sink.write(new Uint8Array(buf)));
      ui.progress(received, meta.size, startedAt);
      if (received === meta.size) await succeed();
    };
    dc.onclose = () => {
      if (!finished) fail('传输中断：发送端连接已关闭');
    };
  }

  async function openRecvSink(name) {
    const dir = state.recvDirHandle;
    if (!dir) {
      const parts = [];
      return {
        kind: 'blob',
        name: name,
        parts: parts,
        write: (chunk) => {
          parts.push(chunk);
          return Promise.resolve();
        },
      };
    }
    const unique = await uniqueFileName(dir, name);
    state.recvNames.add(unique);
    const handle = await dir.getFileHandle(unique, { create: true });
    const writer = await handle.createWritable();
    return {
      kind: 'file',
      name: unique,
      fileHandle: handle,
      write: (chunk) => writer.write(chunk),
      close: () => writer.close(),
      abort: () => writer.abort(),
    };
  }

  function addRecvEntry(ui, item) {
    const entry = { name: item.name, blob: item.blob || null, handle: item.handle || null, li: ui.li };
    ui.onDownload(() => downloadRecvItem(entry));
    state.recvItems.push(entry);
    while (state.recvItems.length > RECV_LIST_MAX) {
      const old = state.recvItems.shift();
      if (old.li && old.li.parentNode) old.li.parentNode.removeChild(old.li);
    }
  }

  async function downloadRecvItem(entry) {
    let blob = entry.blob;
    if (!blob && entry.handle) {
      try {
        blob = await entry.handle.getFile();
      } catch (err) {
        return;
      }
    }
    if (!blob) return;
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = entry.name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 60000);
  }

  async function uniqueFileName(dir, name) {
    const dot = name.lastIndexOf('.');
    const base = dot > 0 ? name.slice(0, dot) : name;
    const ext = dot > 0 ? name.slice(dot) : '';
    let candidate = name;
    for (let i = 1; i <= 999; i++) {
      if (state.recvNames.has(candidate)) {
        candidate = base + ' (' + i + ')' + ext;
        continue;
      }
      try {
        await dir.getFileHandle(candidate);
      } catch (err) {
        if (err && err.name === 'NotFoundError') return candidate;
        throw err;
      }
      candidate = base + ' (' + i + ')' + ext;
    }
    return candidate;
  }

  function sanitizeName(raw) {
    let name = String(raw || '')
      .replace(/^.*[\\/]/, '')
      .replace(/[\x00-\x1f<>:"|?*]/g, '_');
    name = name.replace(/^[. ]+/, '').replace(/[. ]+$/, '');
    if (!name || /^(con|prn|aux|nul|com[1-9]|lpt[1-9])(\..*)?$/i.test(name)) {
      name = 'filet-' + (name || 'download');
    }
    return Array.from(name).slice(0, 150).join('') || 'filet-download';
  }

  function createRecvRowBase(nameText, statusText) {
    const li = document.createElement('li');
    li.className = 'transfer';

    const head = document.createElement('div');
    head.className = 't-head';
    const name = document.createElement('span');
    name.textContent = nameText;
    const status = document.createElement('span');
    status.className = 't-status';
    status.textContent = statusText;
    head.append(name, status);

    const bar = document.createElement('progress');
    bar.max = 100;
    bar.value = 0;

    const meta = document.createElement('div');
    meta.className = 't-meta';

    li.append(head, bar, meta);
    $('#recvList').appendChild(li);

    let last = 0;
    return {
      li: li,
      bar: bar,
      metaEl: meta,
      setStatus(text, cls) {
        status.textContent = text;
        status.className = 't-status' + (cls ? ' ' + cls : '');
      },
      meta(fileName, total) {
        name.textContent = fileName;
        meta.textContent = humanBytes(total);
        status.textContent = '接收中';
        status.className = 't-status';
      },
      progress(received, total, startTime) {
        const now = performance.now();
        if (received < total && now - last < 150) return;
        last = now;
        const pct = total > 0 ? Math.min(100, (received / total) * 100) : 100;
        bar.value = pct;
        const secs = (now - startTime) / 1000;
        const speed = secs > 0 ? received / secs : 0;
        meta.textContent =
          pct.toFixed(1) +
          '% · ' +
          humanBytes(received) +
          ' / ' +
          humanBytes(total) +
          ' · ' +
          humanBytes(speed) +
          '/s';
      },
      onDownload(fn) {
        li.classList.add('clickable');
        li.title = '点击下载';
        li.addEventListener('click', () => {
          fn();
        });
      },
      done(text) {
        bar.value = 100;
        status.textContent = '点击下载';
        status.className = 't-status link';
        meta.textContent = text;
      },
      fail(text) {
        status.textContent = '失败';
        status.className = 't-status bad';
        meta.textContent = text;
      },
    };
  }

  function createReceiveRow() {
    return createRecvRowBase('接收中…', '等待中');
  }

  function createPendingRow(fromName, fileName, size, onAccept, onReject) {
    const safe = sanitizeName(fileName);
    const row = createRecvRowBase('收到文件请求', '待接收');
    row.li.classList.add('pending');
    const info =
      safe + (size > 0 ? ' · ' + humanBytes(size) : '') + (fromName ? ' · 来自 ' + fromName : '');
    row.metaEl.textContent = info;

    const actions = document.createElement('div');
    actions.className = 't-actions';
    const accept = document.createElement('button');
    accept.type = 'button';
    accept.className = 'accept';
    accept.textContent = '接收';
    accept.addEventListener('click', onAccept);
    const reject = document.createElement('button');
    reject.type = 'button';
    reject.className = 'ghost';
    reject.textContent = '拒绝';
    reject.addEventListener('click', onReject);
    actions.append(accept, reject);
    row.li.appendChild(actions);

    row.accepting = () => {
      row.li.classList.remove('pending');
      actions.remove();
      row.setStatus('协商连接…', '');
    };
    row.rejected = () => {
      row.li.classList.remove('pending');
      actions.remove();
      row.bar.remove();
      row.setStatus('已拒绝', 'bad');
    };
    row.expired = () => {
      row.li.classList.remove('pending');
      actions.remove();
      row.bar.remove();
      row.setStatus('已过期', 'bad');
    };
    return row;
  }

  function createTransferRow(dev, file) {
    const li = document.createElement('li');
    li.className = 'transfer';

    const head = document.createElement('div');
    head.className = 't-head';
    const name = document.createElement('span');
    name.textContent = dev.name;
    const status = document.createElement('span');
    status.className = 't-status';
    status.textContent = '等待中';
    head.append(name, status);

    const bar = document.createElement('progress');
    bar.max = 100;
    bar.value = 0;

    const meta = document.createElement('div');
    meta.className = 't-meta';
    meta.textContent = file.name + ' · ' + humanBytes(file.size);

    li.append(head, bar, meta);
    $('#transfers').appendChild(li);

    let last = 0;
    return {
      connecting(text) {
        status.textContent = text;
        status.className = 't-status';
      },
      progress(sent, total, startTime) {
        const now = performance.now();
        if (sent < total && now - last < 150) return;
        last = now;
        const pct = total > 0 ? Math.min(100, (sent / total) * 100) : 100;
        bar.value = pct;
        const secs = (now - startTime) / 1000;
        const speed = secs > 0 ? sent / secs : 0;
        meta.textContent =
          file.name +
          ' · ' +
          pct.toFixed(1) +
          '% · ' +
          humanBytes(sent) +
          ' / ' +
          humanBytes(total) +
          ' · ' +
          humanBytes(speed) +
          '/s';
      },
      done(path) {
        bar.value = 100;
        status.textContent = '完成';
        status.className = 't-status ok';
        meta.textContent = '已保存：' + (path || '（接收端已确认）');
      },
      fail(message) {
        status.textContent = '失败';
        status.className = 't-status bad';
        meta.textContent = message;
      },
    };
  }

  function shortID(id) {
    return id.length > 8 ? id.slice(0, 8) : id;
  }

  function fmtTime(value) {
    const d = new Date(value);
    if (Number.isNaN(d.getTime())) return '';
    return d.toLocaleString('zh-CN', { hour12: false });
  }

  function humanBytes(n) {
    if (n < 1024) return n + ' B';
    const units = ['KB', 'MB', 'GB', 'TB'];
    let i = -1;
    do {
      n /= 1024;
      i++;
    } while (n >= 1024 && i < units.length - 1);
    return n.toFixed(1) + ' ' + units[i];
  }

  function sleep(ms) {
    return new Promise((resolve) => setTimeout(resolve, ms));
  }

  function withTimeout(promise, ms, message) {
    let timer = null;
    const timeout = new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error(message)), ms);
    });
    return Promise.race([promise, timeout]).finally(() => clearTimeout(timer));
  }
})();
