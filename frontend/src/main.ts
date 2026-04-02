const canvas = document.getElementById('game') as HTMLCanvasElement;
const ctx = canvas.getContext('2d')!; //「nullでないことを保証」する非nullアサーション
const statusEl = document.getElementById('status')!;
const timerEl = document.getElementById('timer')!;

// 画面DOM要素
const homeScreen = document.getElementById('homeScreen')!;
const lobbyScreen = document.getElementById('lobbyScreen')!;
const resultScreen = document.getElementById('resultScreen')!;
const gameUi = document.getElementById('ui')!;
const rankingBody = document.getElementById('rankingBody')!;

// NPC確認モーダルのDOM要素
const npcModal = document.getElementById('npcModal')!;
const npcCountStr = document.getElementById('npcCountStr')!;

const WORLD_SIZE = 1000;
let scale = 1;

function resize() {
    canvas.width = window.innerWidth;
    canvas.height = window.innerHeight;
    scale = Math.min(canvas.width, canvas.height) / WORLD_SIZE;
}
window.onresize = resize;
resize();

const myId = Math.random().toString(36).substring(7);
let myName = "ゲスト";
let socket: WebSocket | null = null;
let gameState: any = { players: {}, objects: [], status: 'home', timeLeft: 0 };
let myPos = { x: 500, y: 500 };
let targetPos = { x: 500, y: 500 };
let isInitialized = false;

// 画面切り替えユーティリティ
function showScreen(screenId: string) {
    [homeScreen, lobbyScreen, resultScreen].forEach(el => el.classList.remove('active'));
    gameUi.style.display = 'none';
    
    if (screenId === 'home') homeScreen.classList.add('active');
    else if (screenId === 'lobby') lobbyScreen.classList.add('active');
    else if (screenId === 'result') resultScreen.classList.add('active');
    else if (screenId === 'game') gameUi.style.display = 'block';
}

//サーバーAPIからランキングを取得して表示する非同期関数
async function fetchRanking() {
    try {
        let apiUrl = `http://${window.location.hostname}:8080/api/ranking`;
        if (window.location.hostname.includes('devtunnels.ms')) {
            apiUrl = `https://${window.location.hostname.replace('-5173', '-8080')}/api/ranking`;
        } else if (window.location.protocol === 'https:') {
            apiUrl = `https://${window.location.hostname}:8080/api/ranking`; // スマホ向けフォールバック
        }

        const res = await fetch(apiUrl);//APIリクエスト送信
        const data = await res.json();
        rankingBody.innerHTML = '';
        if(data) {
            data.forEach((r: any, i: number) => {
                rankingBody.innerHTML += `<tr><td>${i+1}</td><td>${r.name}</td><td>${r.points} pt</td></tr>`;
            });
        }
    } catch (e) { console.error("Ranking fetch error", e); }
}
fetchRanking();

// UIイベントリスナー
document.getElementById('joinBtn')!.onclick = () => {
    myName = (document.getElementById('playerName') as HTMLInputElement).value || "ゲスト";
    
    const roomInput = document.getElementById('roomId') as HTMLInputElement;
    const roomId = roomInput ? roomInput.value : "";
    let wsUrl;
    if (window.location.hostname.includes('devtunnels.ms')) {
        wsUrl = `wss://${window.location.hostname.replace('-5173', '-8080')}/ws?id=${myId}&name=${encodeURIComponent(myName)}&room=${encodeURIComponent(roomId)}`;
    } else if (window.location.protocol === 'https:') {
        wsUrl = `wss://${window.location.hostname}:8080/ws?id=${myId}&name=${encodeURIComponent(myName)}&room=${encodeURIComponent(roomId)}`;
    } else {
        wsUrl = `ws://${window.location.hostname}:8080/ws?id=${myId}&name=${encodeURIComponent(myName)}&room=${encodeURIComponent(roomId)}`;
    }
    
    socket = new WebSocket(wsUrl);

    socket.onopen = () => {
        showScreen('lobby');
        socket!.send(JSON.stringify({ type: 'join' }));
    };

    socket.onmessage = (e) => {
        const msg = JSON.parse(e.data);
        
        if (msg.type === 'error') {
            alert(msg.message); 
            showScreen('home'); 
            socket?.close();//?.はnullでも安全に呼び出せるオプショナルチェーン
            return;
        }

        if (msg.type === 'lobby_update') {
            updateLobbyUI(msg.lobby);
        } else if (msg.type === 'game_state') {
            handleGameState(msg.state);
        } else if (msg.type === 'npc_prompt') {
            npcCountStr.innerText = msg.missingCount;
            document.getElementById('npcStatusStr')!.innerText = `現在 ${msg.approvedCount} / ${msg.totalCount} 人が承認`;
            npcModal.style.display = 'block';
        } else if (msg.type === 'cancel_npc_prompt') {
            npcModal.style.display = 'none';
            const yesBtn = document.getElementById('npcYesBtn') as HTMLButtonElement;
            yesBtn.disabled = false;
            yesBtn.innerText = "はい";
        }
    };
};

//NPC追加に「はい」と答えたとき：承認をサーバーに送信してボタンを無効化（二重送信防止）
document.getElementById('npcYesBtn')!.onclick = () => {
    socket?.send(JSON.stringify({ type: 'start_with_npc' }));
    const yesBtn = document.getElementById('npcYesBtn') as HTMLButtonElement;
    yesBtn.disabled = true;
    yesBtn.innerText = "承認済み (待機中...)";
};
document.getElementById('npcNoBtn')!.onclick = () => {
    socket?.send(JSON.stringify({ type: 'cancel_npc' }));
};

document.getElementById('readyBtn')!.onclick = () => {
    socket?.send(JSON.stringify({ type: 'toggle_ready' }));
};

['lobbyDuration', 'lobbyTraps', 'lobbyTarget'].forEach(id => {
    document.getElementById(id)!.addEventListener('change', (e) => {
        let val = parseInt((e.target as HTMLInputElement).value);
        //部屋の人数上限が参加プレイヤー数を下回らないよう制限
        if (id === 'lobbyTarget') {
            const minVal = parseInt((e.target as HTMLInputElement).min);
            if (val < minVal) {
                val = minVal;
                (e.target as HTMLInputElement).value = minVal.toString();
            }
        }
        socket?.send(JSON.stringify({ type: 'update_settings', key: id, value: val }));
    });
});

document.getElementById('backHomeBtn')!.onclick = () => {
    showScreen('home');
    fetchRanking(); 
    socket?.close(); 
    isInitialized = false;
    gameState = { players: {}, objects: [], status: 'home', timeLeft: 0 };
};

function updateLobbyUI(lobby: any) {
    const titleEl = document.getElementById('lobbyTitle');
    if (titleEl && lobby.roomId) {
        titleEl.innerText = `待機室 (部屋: ${lobby.roomId})`;
    }

    const listEl = document.getElementById('playerList')!;
    listEl.innerHTML = '';
    
    let humanCount = 0; 
    for (const id in lobby.players) {
        const p = lobby.players[id];
        if (!p.isBot) humanCount++;//BOTを除いた人間プレイヤー数を集計
        const readyMark = p.isReady ? "✅" : "⏳";
        listEl.innerHTML += `<div>${readyMark} ${p.name} ${id === myId ? '(あなた)' : ''}</div>`;
    }
    
    (document.getElementById('lobbyDuration') as HTMLInputElement).value = lobby.settings.duration;
    (document.getElementById('lobbyTraps') as HTMLInputElement).value = lobby.settings.traps;
    
    const targetEl = document.getElementById('lobbyTarget') as HTMLInputElement;
    if (targetEl) {
        targetEl.min = humanCount.toString();
        targetEl.value = lobby.settings.targetPlayers;
    }
}

//ゲーム状態の変化を処理する関数
function handleGameState(newState: any) {
    //開始
    if (gameState.status !== 'running' && newState.status === 'running') {
        showScreen('game');
        isInitialized = false; 
    }
    //終了
    if (newState.status === 'finished' && gameState.status !== 'finished') {
        timerEl.innerText = `残り: 0s`;
        statusEl.innerText = "タイムアップ！！";
        statusEl.style.color = "#ff4444";
        
        setTimeout(() => {
            showScreen('result');
            const isLoser = newState.players[myId]?.isIt;
            document.getElementById('resultTitle')!.innerText = isLoser ? "YOU LOSE..." : "YOU WIN!";
            
            const resultTbody = document.getElementById('matchResultBody');
            if (resultTbody) {
                resultTbody.innerHTML = '';
                const sortedPlayers = Object.values(newState.players).sort((a: any, b: any) => {
                    if (a.isIt === b.isIt) return 0;
                    return a.isIt ? 1 : -1;
                });

                sortedPlayers.forEach((p: any) => {
                    const award = p.isIt ? "☠️ 鬼" : "👑 逃げ切り";
                    const pts = p.isBot ? "-" : (p.isIt ? "-5" : "+10");
                    const rowStyle = p.id === myId ? "background: #fff3cd; font-weight: bold; color: #333;" : "";
                    const ptColor = p.isIt ? '#e74c3c' : '#27ae60';
                    
                    resultTbody.innerHTML += `<tr style="${rowStyle}"><td>${award}</td><td>${p.name}</td><td style="color: ${ptColor}; font-weight: bold;">${pts}</td></tr>`;
                });
            }
            statusEl.style.color = "#fff";
        }, 1500);
    }

    if (newState.players[myId]) {
        if (!isInitialized) {
            myPos.x = newState.players[myId].x;
            myPos.y = newState.players[myId].y;
            targetPos = { ...myPos };
            isInitialized = true;
        }
        
        if (newState.players[myId].isStunned) {
            myPos.x = newState.players[myId].x;
            myPos.y = newState.players[myId].y;
        }
    }
    
    gameState = newState;
    
    if (gameState.status === 'running') {
        timerEl.innerText = `残り: ${gameState.timeLeft}s`;
        statusEl.innerText = "鬼から逃げろ！";
    }
}

//マウス座標→ゲーム仮想座標に変換する関数
function getVirtualCoords(clientX: number, clientY: number) {
    const rect = canvas.getBoundingClientRect();
    const offsetX = (canvas.width - WORLD_SIZE * scale) / 2;
    const offsetY = (canvas.height - WORLD_SIZE * scale) / 2;
    let x = (clientX - rect.left - offsetX) / scale;
    let y = (clientY - rect.top - offsetY) / scale;
    
    x = Math.max(15, Math.min(x, WORLD_SIZE - 15));
    y = Math.max(15, Math.min(y, WORLD_SIZE - 15));
    return { x, y };
}

window.addEventListener('mousemove', (e) => {
    const coords = getVirtualCoords(e.clientX, e.clientY);
    targetPos.x = coords.x;
    targetPos.y = coords.y;
});

//スマホのためのタッチ操作対応
const handleTouch = (e: TouchEvent) => {
    if (e.touches.length > 0) {
        e.preventDefault(); 
        const coords = getVirtualCoords(e.touches[0].clientX, e.touches[0].clientY);
        targetPos.x = coords.x;
        targetPos.y = coords.y;
    }
};
canvas.addEventListener('touchstart', handleTouch, { passive: false });
canvas.addEventListener('touchmove', handleTouch, { passive: false });

// ロジック & 描画
function update() {
    if (!isInitialized || gameState.players[myId]?.isStunned || gameState.status !== 'running') return;

    const speed = 5;
    const dx = targetPos.x - myPos.x;
    const dy = targetPos.y - myPos.y;
    const dist = Math.sqrt(dx * dx + dy * dy);

    if (dist > 1) {
        if (dist > speed) {
            myPos.x += (dx / dist) * speed;
            myPos.y += (dy / dist) * speed;
        } else {
            myPos.x = targetPos.x;
            myPos.y = targetPos.y;
        }
    }

    //障害物との衝突判定
    gameState.objects.forEach((obj: any) => {
        if (obj.type === 'obstacle') {
            const odx = myPos.x - obj.x;
            const ody = myPos.y - obj.y;
            const distToObj = Math.sqrt(odx * odx + ody * ody);
            const minDist = obj.radius + 15;

            if (distToObj < minDist) {
                const safeDist = distToObj === 0 ? 0.1 : distToObj;
                myPos.x = obj.x + (odx / safeDist) * minDist;
                myPos.y = obj.y + (ody / safeDist) * minDist;
            }
        }
    });

    myPos.x = Math.max(15, Math.min(myPos.x, WORLD_SIZE - 15));
    myPos.y = Math.max(15, Math.min(myPos.y, WORLD_SIZE - 15));

    if (socket?.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({ type: 'move', x: myPos.x, y: myPos.y }));
    }
}

//星形を描画する関数
function drawStar(cx: number, cy: number, spikes: number, outer: number, inner: number) {
    let rot = Math.PI / 2 * 3;
    let step = Math.PI / spikes;
    ctx.beginPath();
    ctx.moveTo(cx, cy - outer);
    for (let i = 0; i < spikes; i++) {
        ctx.lineTo(cx + Math.cos(rot) * outer, cy + Math.sin(rot) * outer);
        rot += step;
        ctx.lineTo(cx + Math.cos(rot) * inner, cy + Math.sin(rot) * inner);
        rot += step;
    }
    ctx.closePath();
    ctx.fill();
}

//メイン描画関数
function draw() {
    update();
    const offsetX = (canvas.width - WORLD_SIZE * scale) / 2;
    const offsetY = (canvas.height - WORLD_SIZE * scale) / 2;

    ctx.clearRect(0, 0, canvas.width, canvas.height);

    if (gameState.status !== 'running') {
        requestAnimationFrame(draw);
        return;
    }

    ctx.save();
    ctx.translate(offsetX, offsetY);

    ctx.fillStyle = '#e0e0e0';
    ctx.fillRect(0, 0, WORLD_SIZE * scale, WORLD_SIZE * scale);

    gameState.objects.forEach((obj: any) => {
        if (obj.type === 'stun') {
            ctx.fillStyle = '#FFD700';
            drawStar(obj.x * scale, obj.y * scale, 8, obj.radius * scale, (obj.radius / 2) * scale);
        } else {
            ctx.fillStyle = obj.type === 'blind' ? '#4B0082' : '#777';
            ctx.beginPath();
            ctx.arc(obj.x * scale, obj.y * scale, obj.radius * scale, 0, Math.PI * 2);
            ctx.fill();
        }
    });

    for (const id in gameState.players) {
        const p = gameState.players[id];
        const isMe = id === myId;
        let drawX = (isMe ? myPos.x : p.x) * scale;
        let drawY = (isMe ? myPos.y : p.y) * scale;

        if (p.isStunned) {
            drawX += (Math.random() - 0.5) * 10;
            drawY += (Math.random() - 0.5) * 10;
        }

        ctx.beginPath();
        ctx.arc(drawX, drawY, 15 * scale, 0, Math.PI * 2);
        ctx.fillStyle = p.isIt ? '#ff4444' : '#4444ff';
        ctx.fill();
        
        if (isMe) {
            ctx.strokeStyle = '#000';
            ctx.lineWidth = 2;
            ctx.stroke();
        }
    }
    ctx.restore();

    if (gameState.players[myId]?.isBlind) {
        ctx.save();
        const screenX = myPos.x * scale + offsetX;
        const screenY = myPos.y * scale + offsetY;
        const viewRadius = 120 * scale;

        ctx.fillStyle = "black";
        ctx.beginPath();
        ctx.rect(0, 0, canvas.width, canvas.height);
        ctx.moveTo(screenX + viewRadius, screenY);
        ctx.arc(screenX, screenY, viewRadius, 0, Math.PI * 2, true);
        ctx.fill();
        ctx.restore();
    }

    requestAnimationFrame(draw);
}

draw();