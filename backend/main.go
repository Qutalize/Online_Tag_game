package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
	"github.com/gorilla/websocket"
)

//ゲームの定数定義
const WorldSize = 1000.0
const TagCooldown = 5 * time.Second  //タグ後の無敵時間
const StunDuration = 5 * time.Second //スタン継続時間
const BlindDuration = 5 * time.Second

//フィールド上のオブジェクト（障害物・トラップ）
type Object struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Radius float64 `json:"radius"`
	Type   string  `json:"type"` // "obstacle" / "stun" / "blind"
}

//プレイヤーの状態
type Player struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	IsBot       bool      `json:"isBot"`
	IsReady     bool      `json:"isReady"`
	NpcApproved bool      `json:"npcApproved"` //NPC追加への同意フラグ
	X           float64   `json:"x"`
	Y           float64   `json:"y"`
	IsIt        bool      `json:"isIt"`      //鬼かどうか
	IsStunned   bool      `json:"isStunned"`
	IsBlind     bool      `json:"isBlind"`
	ImmuneUntil time.Time `json:"-"` //鬼返しなどの防止のための無敵時間
	StunUntil   time.Time `json:"-"`
	BlindUntil  time.Time `json:"-"`
}

//ロビーの設定値
type LobbySettings struct {
	TargetPlayers int `json:"targetPlayers"` //ゲーム開始に必要な人数
	Duration      int `json:"duration"`      //ゲーム時間（秒）
	Traps         int `json:"traps"`         //トラップ数
}

//部屋単位のゲーム全体の状態
//sync.Mutexで複数のgoroutineから安全にアクセスできるよう排他制御する
type Game struct {
	ID        string             `json:"roomId"`
	Players   map[string]*Player `json:"players"`
	Objects   []Object           `json:"objects"`
	Status    string             `json:"status"`   // "lobby" / "running" / "finished"
	TimeLeft  int                `json:"timeLeft"`
	Settings  LobbySettings      `json:"-"`
	mu        sync.Mutex                    //Players・Objects・Status の読み書きを保護
	clients   map[*websocket.Conn]string    //接続中のWebSocketとプレイヤーIDのマップ
	clientsMu sync.Mutex                    //clientsマップの読み書きを保護
}

//全部屋を管理するグローバルマップと、その排他制御用Mutex
var games = make(map[string]*Game)
var gamesMu sync.Mutex

//WebSocketへのアップグレード設定（CORSは全オリジン許可）
var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

//データベース（JSONファイルによる永続化）
type DBRecord struct {
	Name   string `json:"name"`
	Points int    `json:"points"`
}
var dbFile = "database.json"
var dbData = make(map[string]*DBRecord)
var dbMu sync.Mutex

//起動時にJSONファイルからランキングデータを読み込む
func loadDB() {
	file, err := os.ReadFile(dbFile)
	if err == nil { json.Unmarshal(file, &dbData) }
}

//ゲーム終了時にランキングデータをJSONファイルへ書き出す
func saveDB() {
	data, _ := json.MarshalIndent(dbData, "", "  ")
	os.WriteFile(dbFile, data, 0644)
}

func main() {
	rand.Seed(time.Now().UnixNano()) //乱数シードを初期化
	loadDB()

	http.HandleFunc("/api/ranking", handleRanking)
	http.HandleFunc("/ws", handleConnections)

	log.Println("Server started on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

//ランキングAPIハンドラ：ポイント降順でJSONを返す
func handleRanking(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*") //フロントからのCORSリクエストを許可
	w.Header().Set("Content-Type", "application/json")
	var ranks []DBRecord
	dbMu.Lock()
	for _, record := range dbData { ranks = append(ranks, *record) }
	dbMu.Unlock()
	sort.Slice(ranks, func(i, j int) bool { return ranks[i].Points > ranks[j].Points })
	json.NewEncoder(w).Encode(ranks)
}

//目標人数が人間プレイヤー数・最低2人を下回らないよう補正
func adjustTargetPlayers(g *Game) {
	humanCount := 0
	for _, p := range g.Players {
		if !p.IsBot { humanCount++ }
	}
	if g.Settings.TargetPlayers < humanCount {
		g.Settings.TargetPlayers = humanCount
	}
	if g.Settings.TargetPlayers < 2 {
		g.Settings.TargetPlayers = 2
	}
}

//WebSocket接続ハンドラ：1接続につき1つのgoroutineで動く
func handleConnections(w http.ResponseWriter, r *http.Request) {
	//HTTPをWebSocketにアップグレード
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil { return }

	id := r.URL.Query().Get("id")
	name := r.URL.Query().Get("name")
	if name == "" { name = "ゲスト" }
	roomID := r.URL.Query().Get("room")

	//部屋IDが空なら4桁ランダムIDで新規作成、既存部屋なら参加
	gamesMu.Lock()
	if roomID == "" {
		roomID = fmt.Sprintf("%04d", rand.Intn(10000))
	}
	g, ok := games[roomID]
	if !ok {
		g = &Game{
			ID:       roomID,
			Players:  make(map[string]*Player),
			Status:   "lobby",
			Settings: LobbySettings{TargetPlayers: 5, Duration: 60, Traps: 50},
			clients:  make(map[*websocket.Conn]string),
		}
		games[roomID] = g
		go gameLoop(g) //部屋ごとにゲームループをgoroutineで起動
	}
	gamesMu.Unlock()

	g.mu.Lock()
	//ゲーム進行中は参加を拒否
	if g.Status == "running" {
		g.mu.Unlock()
		msg, _ := json.Marshal(map[string]interface{}{
			"type":    "error",
			"message": "現在ゲームが進行中です。終了するまで参加できません。",
		})
		ws.WriteMessage(websocket.TextMessage, msg)
		ws.Close()
		return
	}

	//終了済みの部屋に参加した場合はロビーにリセット
	if g.Status == "finished" {
		g.Status = "lobby"
		g.Objects = []Object{}
		for pid, p := range g.Players {
			if p.IsBot {
				delete(g.Players, pid) //ボットは削除
			} else {
				p.IsReady = false      //人間は準備状態をリセット
			}
		}
	}

	g.Players[id] = &Player{ID: id, Name: name, X: 500, Y: 500}
	adjustTargetPlayers(g)
	g.mu.Unlock()

	g.clientsMu.Lock()
	g.clients[ws] = id //WebSocket接続をプレイヤーIDと紐付けて登録
	g.clientsMu.Unlock()

	broadcastLobby(g)

	//切断時のクリーンアップ
	defer func() {
		g.mu.Lock()
		delete(g.Players, id)

		humanCount := 0
		for _, p := range g.Players {
			if !p.IsBot { humanCount++ }
		}
		//人間プレイヤーが0人になったら部屋ごと削除してメモリを解放
		if humanCount == 0 {
			g.mu.Unlock()
			gamesMu.Lock()
			delete(games, g.ID)
			gamesMu.Unlock()
		} else {
			adjustTargetPlayers(g)
			g.mu.Unlock()
			broadcastLobby(g)
		}

		g.clientsMu.Lock()
		delete(g.clients, ws)
		g.clientsMu.Unlock()
		ws.Close()
	}()

	//メッセージ受信ループ
	for {
		var msg map[string]interface{}
		if err := ws.ReadJSON(&msg); err != nil { break }
		msgType, _ := msg["type"].(string)

		switch msgType {
		case "toggle_ready":
			g.mu.Lock()
			if p, ok := g.Players[id]; ok {
				p.IsReady = !p.IsReady
				if !p.IsReady { p.NpcApproved = false } //準備解除時はNPC同意もリセット
			}
			g.mu.Unlock()
			checkAndStartGame(g)
			broadcastLobby(g)

		case "start_with_npc": //NPC追加に同意
			g.mu.Lock()
			if p, ok := g.Players[id]; ok { p.NpcApproved = true }
			g.mu.Unlock()
			checkAndStartGame(g)

		case "cancel_npc": //NPC追加をキャンセル：全員の準備状態をリセット
			g.mu.Lock()
			for _, p := range g.Players {
				if !p.IsBot {
					p.IsReady = false
					p.NpcApproved = false
				}
			}
			g.mu.Unlock()
			broadcastLobby(g)
			msg, _ := json.Marshal(map[string]interface{}{"type": "cancel_npc_prompt"})
			sendToAll(g, msg)

		case "update_settings":
			key, _ := msg["key"].(string)
			valFloat, _ := msg["value"].(float64)
			g.mu.Lock()
			if key == "lobbyDuration" { g.Settings.Duration = int(valFloat) }
			if key == "lobbyTraps" { g.Settings.Traps = int(valFloat) }
			if key == "lobbyTarget" {
				g.Settings.TargetPlayers = int(valFloat)
				adjustTargetPlayers(g)
			}
			g.mu.Unlock()
			broadcastLobby(g)

		case "move":
			if g.Status != "running" { continue }
			x, _ := msg["x"].(float64)
			y, _ := msg["y"].(float64)
			g.mu.Lock()
			//スタン中は移動不可
			if p, ok := g.Players[id]; ok && !p.IsStunned {
				p.X = math.Max(15, math.Min(x, WorldSize-15))
				p.Y = math.Max(15, math.Min(y, WorldSize-15))
			}
			g.mu.Unlock()
		}
	}
}

//ロビー状態を全クライアントに送信する
func broadcastLobby(g *Game) {
	g.mu.Lock()
	if g.Status != "lobby" { // ゲーム中・終了後は送信しない
		g.mu.Unlock()
		return
	}
	payload := map[string]interface{}{
		"type": "lobby_update",
		"lobby": map[string]interface{}{
			"roomId":   g.ID,
			"players":  g.Players,
			"settings": g.Settings,
		},
	}
	msg, _ := json.Marshal(payload)
	g.mu.Unlock()
	sendToAll(g, msg)
}

//全員が準備完了かチェックし、条件を満たせばゲームを開始する
func checkAndStartGame(g *Game) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.Status == "running" { return }

	//全人間プレイヤーが準備完了かチェック（1人でも未完了なら中断）
	humanCount := 0
	for _, p := range g.Players {
		if !p.IsBot {
			humanCount++
			if !p.IsReady { return }
		}
	}
	if humanCount == 0 { return }

	target := g.Settings.TargetPlayers

	//人数が目標に達していない場合、NPC追加の確認プロンプトを送る
	if humanCount < target {
		approvedCount := 0
		for _, p := range g.Players {
			if !p.IsBot && p.NpcApproved { approvedCount++ }
		}

		//全員が同意していなければ確認メッセージを送って待機
		if approvedCount < humanCount {
			payload := map[string]interface{}{
				"type":          "npc_prompt",
				"missingCount":  target - humanCount,
				"approvedCount": approvedCount,
				"totalCount":    humanCount,
			}
			msg, _ := json.Marshal(payload)
			go sendToAll(g, msg)
			return
		}
	}

	//目標人数に達するまでボットを追加
	botCount := 1
	for len(g.Players) < target {
		botID := fmt.Sprintf("bot-%d", botCount)
		botName := fmt.Sprintf("NPC-%c", 'A'+botCount-1) // NPC-A, NPC-B, ...
		g.Players[botID] = &Player{
			ID: botID, Name: botName, IsBot: true, IsReady: true,
		}
		botCount++
	}

	//全プレイヤーの状態を初期化（開始から3秒間は無敵）
	for _, p := range g.Players {
		p.NpcApproved = false
		p.IsStunned = false
		p.IsBlind = false
		p.StunUntil = time.Time{}
		p.BlindUntil = time.Time{}
		p.ImmuneUntil = time.Now().Add(time.Second * 3)
	}

	g.Status = "running"
	g.TimeLeft = g.Settings.Duration
	g.Objects = []Object{}

	//ランダムに1人を鬼に選ぶ
	itAssigned := false
	for _, p := range g.Players {
		p.X, p.Y = rand.Float64()*WorldSize, rand.Float64()*WorldSize
		p.IsIt = false
		if !itAssigned && rand.Intn(2) == 0 {
			p.IsIt = true
			itAssigned = true
		}
	}
	//ループで偶然誰も選ばれなかった場合、先頭の1人を鬼にする
	if !itAssigned {
		for _, p := range g.Players {
			p.IsIt = true
			break
		}
	}

	//障害物を5つ配置
	for i := 0; i < 8; i++ {
		g.Objects = append(g.Objects, Object{X: rand.Float64()*WorldSize, Y: rand.Float64()*WorldSize, Radius: 40, Type: "obstacle"})
	}
	//トラップをランダムにstun/blindで配置
	for i := 0; i < g.Settings.Traps; i++ {
		tType := "stun"
		if rand.Intn(2) == 0 { tType = "blind" }
		g.Objects = append(g.Objects, Object{X: rand.Float64()*WorldSize, Y: rand.Float64()*WorldSize, Radius: 20, Type: tType})
	}

	//NPC確認モーダルをクライアント側で閉じさせる
	msg, _ := json.Marshal(map[string]interface{}{"type": "cancel_npc_prompt"})
	go sendToAll(g, msg)
}

//部屋ごとのゲームループ（goroutineで常駐）
func gameLoop(g *Game) {
	ticker := time.NewTicker(33 * time.Millisecond)//約30fpsでゲーム状態更新＆ブロードキャスト
	secTicker := time.NewTicker(1 * time.Second)//1秒ごとにタイマーを減算してゲーム終了を判定
	defer ticker.Stop()
	defer secTicker.Stop()

	for {
		select {
		case <-ticker.C:
			g.mu.Lock()
			humanCount := 0
			for _, p := range g.Players {
				if !p.IsBot { humanCount++ }
			}
			//人間が全員退出したらgoroutineを終了（メモリリーク防止）
			if humanCount == 0 {
				g.mu.Unlock()
				return
			}

			isRunning := (g.Status == "running")
			g.mu.Unlock()

			if isRunning {
				updateGame(g)
				broadcastGame(g)
			}
		case <-secTicker.C:
			var justFinished bool

			g.mu.Lock()
			if g.Status == "running" && g.TimeLeft > 0 {
				g.TimeLeft--
				if g.TimeLeft <= 0 {
					g.TimeLeft = 0
					g.Status = "finished"
					justFinished = true

					//ゲーム終了時にポイントを集計して保存
					dbMu.Lock()
					for _, p := range g.Players {
						if p.IsBot { continue } //ボットはポイント対象外
						if dbData[p.Name] == nil {
							dbData[p.Name] = &DBRecord{Name: p.Name, Points: 0}
						}
						if !p.IsIt {
							dbData[p.Name].Points += 10 //逃げ切り成功
						} else {
							dbData[p.Name].Points -= 5  //鬼のまま終了
						}
					}
					saveDB()
					dbMu.Unlock()
				}
			}
			g.mu.Unlock()

			//ロック外でブロードキャスト（終了状態を即座に全員へ通知）
			if justFinished {
				broadcastGame(g)
			}
		}
	}
}

//1フレーム分のゲームロジックを更新する
func updateGame(g *Game) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()

	var it *Player //現在の鬼を探す
	for _, p := range g.Players {
		if p.IsIt { it = p }

		//スタン・ブラインドは時刻で管理し、期限切れを自動解除
		p.IsStunned = now.Before(p.StunUntil)
		p.IsBlind = now.Before(p.BlindUntil)

		//トラップ・障害物との当たり判定（後ろから削除するためインデックスを逆順に走査）
		for i := len(g.Objects) - 1; i >= 0; i-- {
			obj := g.Objects[i]
			dx := p.X - obj.X
			dy := p.Y - obj.Y
			dist := math.Sqrt(dx*dx + dy*dy)
			minDist := obj.Radius + 15 //オブジェクト半径+プレイヤー半径

			if dist < minDist {
				if obj.Type == "obstacle" {
					//障害物：めり込まないよう外側に押し戻す
					if dist == 0 {
						dx, dy = rand.Float64()-0.5, rand.Float64()-0.5
						dist = math.Hypot(dx, dy)
					}
					p.X = obj.X + (dx/dist)*minDist
					p.Y = obj.Y + (dy/dist)*minDist
				} else if obj.Type == "stun" {
					p.StunUntil = now.Add(StunDuration)
					p.IsStunned = true
					g.Objects = append(g.Objects[:i], g.Objects[i+1:]...) //トラップを消費・削除
				} else if obj.Type == "blind" {
					p.BlindUntil = now.Add(BlindDuration)
					p.IsBlind = true
					g.Objects = append(g.Objects[:i], g.Objects[i+1:]...)
				}
			}
		}
	}

	//タグ判定：鬼が他プレイヤーに30以内に近づいたら鬼を交代
	if it != nil && now.After(it.ImmuneUntil) {
		for _, p := range g.Players {
			if p.ID != it.ID && now.After(p.ImmuneUntil) {
				dist := math.Hypot(it.X-p.X, it.Y-p.Y)
				if dist < 30 {
					it.IsIt, p.IsIt = false, true           //鬼を交代
					it.ImmuneUntil = now.Add(TagCooldown)   //元鬼に無敵付与
					p.ImmuneUntil = now.Add(TagCooldown)    //新鬼にも無敵付与
					break
				}
			}
		}
	}

	//ボットのAI処理
	for _, p := range g.Players {
		if !p.IsBot || p.IsStunned { continue }

		speed := 5.0
		if p.IsIt {
			//鬼ボット：最も近い非鬼プレイヤーを追いかける
			var target *Player
			minDist := math.MaxFloat64
			for _, other := range g.Players {
				if other.ID == p.ID || other.IsIt || now.Before(other.ImmuneUntil) {
					continue
				}
				dist := math.Hypot(other.X-p.X, other.Y-p.Y)
				if dist < minDist {
					minDist = dist
					target = other
				}
			}
			if target != nil {
				dx := target.X - p.X
				dy := target.Y - p.Y
				if minDist == 0 || (dx == 0 && dy == 0) {
					p.X += (rand.Float64() - 0.5) * speed //重なった場合はランダム移動
				} else {
					p.X += (dx / minDist) * speed
					p.Y += (dy / minDist) * speed
				}
			}
		} else if it != nil {
			//逃げボット：鬼が300以内に近づいたら逃げる
			dist := math.Hypot(it.X-p.X, it.Y-p.Y)
			if dist < 300 {
				dx := p.X - it.X //鬼と逆方向へ
				dy := p.Y - it.Y
				if dist == 0 || (dx == 0 && dy == 0) {
					p.X += (rand.Float64() - 0.5) * speed
					p.Y += (rand.Float64() - 0.5) * speed
				} else {
					p.X += (dx / dist) * speed
					p.Y += (dy / dist) * speed
				}
			}
		}

		p.X = math.Max(15, math.Min(p.X, WorldSize-15))
		p.Y = math.Max(15, math.Min(p.Y, WorldSize-15))
	}
}

//ゲーム状態を全クライアントへブロードキャスト
func broadcastGame(g *Game) {
	g.mu.Lock()
	payload := map[string]interface{}{"type": "game_state", "state": g}
	msg, _ := json.Marshal(payload)
	g.mu.Unlock()
	sendToAll(g, msg)
}

//全接続クライアントにメッセージを送信
func sendToAll(g *Game, msg []byte) {
	g.clientsMu.Lock()
	defer g.clientsMu.Unlock()
	for ws := range g.clients {
		ws.WriteMessage(websocket.TextMessage, msg)
	}
}