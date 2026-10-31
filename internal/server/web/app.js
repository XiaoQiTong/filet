'use strict';

(function () {
  const $ = (sel) => document.querySelector(sel);

  const state = {
    ws: null,
    devices: [],
    ice: [],
    file: null,
    loggedIn: false,
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
    recvNames: new Set(),
    recvItems: [],
  };

  const RECV_LIST_MAX = 12;

  document.addEventListener('DOMContentLoaded', init);

  function init() {
    bindAuth();
    bindMain();
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
    $('#auth').hidden = true;
    $('#app').hidden = false;
    $('#whoami').textContent = username;
    renderReceivePanel();
    connectWS();
    loadDevices();
  }

  function showAuth() {
    state.loggedIn = false;
    if (state.receive) {
      state.receive = false;
      state.recvDirHandle = null;
      state.recvNames.clear();
      state.recvItems = [];
      for (const from of Array.from(state.recvPeers.keys())) closeRecvPeer(from);
      state.recvCandidates.clear();
      $('#recvToggle').textContent = '接收文件';
      $('#recvPanel').hidden = true;
      $('#recvList').textContent = '';
    }
    if (state.ws) {
      try {
        state.ws.close();
      } catch (err) {
        /* ignore */
      }
      state.ws = null;
    }
    state.devices = [];
    $('#app').hidden = true;
    $('#auth').hidden = false;
    $('#password').value = '';
  }

  function connectWS() {
    if (!state.loggedIn) return;
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
        const handler = state.pending.get(msg.from);
        if (handler) {
          handler(msg.data);
          return;
        }
        handleIncomingSignal(msg.from, msg.data);
      } else if (msg.type === 'error') {
        console.warn('服务端提示:', msg.error);
      }
    };
    ws.onclose = async () => {
      if (state.ws === ws) state.ws = null;
      if (!state.loggedIn) return;
      await sleep(2000);
      if (!state.loggedIn) return;
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
      const li = document.createElement('li');
      li.className = 'device' + (dev.online && !own ? '' : ' offline');

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

      const badge = document.createElement('span');
      badge.className = 'badge ' + (own ? 'web' : dev.online ? 'on' : 'off');
      badge.textContent = own ? '本页面' : dev.online ? '在线' : '离线';

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
    $('#recvToggle').addEventListener('click', () => {
      if (state.receive) {
        disableReceive();
      } else {
        enableReceive();
      }
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
        answerReject(new Error('目标设备未开启接收'));
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
      ui.connecting('协商连接…');
      await signalSend(dev.id, {
        kind: 'offer',
        description: { type: pc.localDescription.type, sdp: pc.localDescription.sdp },
      });

      const answer = await withTimeout(
        answerPromise,
        30000,
        '等待设备应答超时：设备可能不在线，或双方 NAT 无法穿透'
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

  function enableReceive() {
    if (!state.loggedIn || state.receive) return;
    ensureRecvIdentity();
    const label = browserLabel();
    state.recvName = label + ' 页面';
    state.recvPlatform = 'web/' + label.toLowerCase();
    state.receive = true;
    $('#recvToggle').textContent = '停止接收';
    renderReceivePanel();
    reconnectWS();
  }

  function disableReceive() {
    if (!state.receive) return;
    state.receive = false;
    state.recvDirHandle = null;
    state.recvNames.clear();
    state.recvItems = [];
    for (const from of Array.from(state.recvPeers.keys())) closeRecvPeer(from);
    state.recvCandidates.clear();
    $('#recvToggle').textContent = '接收文件';
    $('#recvPanel').hidden = true;
    $('#recvList').textContent = '';
    if (state.loggedIn) reconnectWS();
  }

  function reconnectWS() {
    if (!state.loggedIn) return;
    const old = state.ws;
    state.ws = null;
    if (old) {
      try {
        old.close();
      } catch (err) {
        /* ignore */
      }
    }
    connectWS();
  }

  function renderReceivePanel() {
    const panel = $('#recvPanel');
    if (!state.receive) {
      panel.hidden = true;
      return;
    }
    panel.hidden = false;
    const canPick = typeof window.showDirectoryPicker === 'function';
    const pick = $('#pickDir');
    pick.hidden = !canPick;
    pick.textContent = state.recvDirHandle ? '更换目录' : '选择保存目录';
    let text =
      '接收已开启：' +
      state.recvName +
      ' · 保存到 ' +
      (state.recvDirHandle ? '「' + state.recvDirHandle.name + '」' : '浏览器默认下载目录') +
      '。保持本页面打开，别人即可把文件发到这台设备。';
    if (!state.recvDirHandle) {
      text += canPick
        ? ' 未选择目录时文件会先缓存在内存，大文件建议先选目录。'
        : ' 当前浏览器不支持目录直写，文件会先缓存在内存，超大文件可能失败。';
    }
    text += ' 收到的文件会立即出现在下方列表，点击即可下载（最多保留 ' + RECV_LIST_MAX + ' 条）。';
    $('#recvStatus').textContent = text;
  }

  function handleIncomingSignal(from, data) {
    if (!data) return;
    if (data.kind === 'offer' && data.description) {
      if (!state.receive) {
        signalSend(from, { kind: 'reject' }).catch(() => {});
        return;
      }
      handleOffer(from, data.description).catch((err) => {
        console.warn('处理来电失败:', err);
        signalSend(from, { kind: 'reject' }).catch(() => {});
      });
      return;
    }
    if (data.kind === 'candidate' && data.candidate) {
      addRecvCandidate(from, data.candidate);
    }
  }

  async function handleOffer(from, description) {
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
      receiveDataChannel(ev.channel, from);
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

  async function receiveDataChannel(dc, from) {
    const ui = createReceiveRow();
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
        setTimeout(() => closeRecvPeer(from), 1000);
        return;
      }
      try {
        dc.send(JSON.stringify({ kind: 'done', ok: true, path: sink.name || '' }));
      } catch (err) {
        /* ignore */
      }
      setTimeout(() => closeRecvPeer(from), 1000);
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
      setTimeout(() => closeRecvPeer(from), 1000);
    };

    dc.binaryType = 'arraybuffer';
    dc.onmessage = async (ev) => {
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

  function createReceiveRow() {
    const li = document.createElement('li');
    li.className = 'transfer';

    const head = document.createElement('div');
    head.className = 't-head';
    const name = document.createElement('span');
    name.textContent = '接收中…';
    const status = document.createElement('span');
    status.className = 't-status';
    status.textContent = '等待中';
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
