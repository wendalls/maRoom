// main.go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Room 房间结构
type Room struct {
	ID            string
	Name          string
	clients       map[*Client]bool // 当前连接
	allowedUsers  map[string]bool  // 允许的用户ID（最多2个）
	userConnCount map[string]int   // 每个用户ID的连接数
	playerNumbers map[string]int   // 用户ID -> 玩家编号 (1或2)
	messages      []Message
	createdAt     time.Time
	mu            sync.RWMutex
	GameState
}

// Client 客户端结构
type Client struct {
	conn         *websocket.Conn
	username     string
	userID       string
	color        string
	roomID       string
	playerNumber int // 玩家编号 (1或0)
	send         chan []byte
	isHome       bool // 是否为主页客户端
}

// Message 消息结构
type Message struct {
	Type         string    `json:"type"` // message, system, userlist, history, roomlist, error, gameState, playerNumber
	UserID       string    `json:"userID,omitempty"`
	Username     string    `json:"username,omitempty"`
	PlayerNumber int       `json:"playerNumber,omitempty"`
	Content      string    `json:"content,omitempty"`
	Time         time.Time `json:"time"`
	Color        string    `json:"color,omitempty"`
	RoomID       string    `json:"roomID,omitempty"`
}

// GameState 游戏状态
type GameState struct {
	Deck        []int  `json:"deck"`
	Extra       []int  `json:"extra"`
	Hands       []int  `json:"hands"`
	Extra1      []int  `json:"extra1"`
	Hands1      []int  `json:"hands1"`
	Discard     []int  `json:"discard"`
	CurrentTurn int    `json:"currentTurn"` // 当前回合玩家编号 (1或0)
	LittleTurn  int    `json:"littleTurn"`
	GameAction  string `json:"gameAction"`
	IsNew       bool   `json:"isNew"`
}

var (
	roomsMu         sync.RWMutex
	rooms           = make(map[string]*Room) // 房间ID -> 房间
	homeClients     = make(map[*Client]bool) // 主页WebSocket连接
	homeClientsMu   sync.RWMutex
	maxHistory      = 100
	maxUsersPerRoom = 2 // 每个房间最多2个不同用户
	roomNames       = []string{
		"双人闲聊室", "游戏对战厅", "学习交流角", "音乐分享坊", "电影讨论屋",
		"美食推荐街", "旅行计划站", "技术交流馆", "读书分享会", "运动健身场",
		"工作协作间", "艺术欣赏厅", "宠物交流园", "深夜谈心台", "好心情树洞",
	}
)

func main() {
	// 初始化随机种子
	rand.Seed(time.Now().UnixNano())

	http.HandleFunc("/", homeHandler)
	http.HandleFunc("/chat", chatHandler)
	http.HandleFunc("/ws", handleConnections)
	http.HandleFunc("/ws-home", handleHomeConnections)
	http.HandleFunc("/api/create-room", createRoomHandler)
	http.HandleFunc("/api/rooms", listRoomsHandler)

	certFile := "cert.pem"   // 证书文件路径
	keyFile := "private.key" // 私钥文件路径
	log.Println("服务器启动: http://localhost:8080")
	//log.Fatal(http.ListenAndServe(":8080", nil))
	http.ListenAndServeTLS(":8080", certFile, keyFile, nil)
}

// 主页处理器
func homeHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

// 聊天室页面处理器
func chatHandler(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	http.ServeFile(w, r, "chat.html")
}

// 处理主页WebSocket连接
func handleHomeConnections(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("主页WebSocket升级失败: %v", err)
		return
	}

	client := &Client{
		conn:   ws,
		send:   make(chan []byte, 256),
		isHome: true,
	}

	homeClientsMu.Lock()
	homeClients[client] = true
	homeClientsMu.Unlock()

	// 发送当前房间列表
	sendRoomListToClient(client)

	// 启动写入goroutine
	go client.writePump()

	// 读取消息（主页客户端只接收，不发送消息）
	client.readHomePump()
}

func (c *Client) readHomePump() {
	defer func() {
		homeClientsMu.Lock()
		delete(homeClients, c)
		homeClientsMu.Unlock()
		close(c.send)
		c.conn.Close()
	}()

	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("主页读取错误: %v", err)
			}
			break
		}
	}
}

// 处理聊天室WebSocket连接
func handleConnections(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket升级失败: %v", err)
		ws.Close()
		return
	}

	// 等待客户端发送连接信息
	_, msgBytes, err := ws.ReadMessage()
	if err != nil {
		log.Printf("读取连接信息失败: %v", err)
		ws.Close()
		return
	}

	var connectMsg struct {
		Type     string `json:"type"`
		RoomID   string `json:"roomID"`
		UserID   string `json:"userID"`
		Username string `json:"username"`
	}

	if err := json.Unmarshal(msgBytes, &connectMsg); err != nil {
		log.Printf("解析连接信息失败: %v", err)
		ws.Close()
		return
	}

	// 获取房间
	roomsMu.RLock()
	room, exists := rooms[connectMsg.RoomID]
	roomsMu.RUnlock()

	if !exists {
		sendError(ws, "房间不存在")
		return
	}

	// 检查房间是否已满
	room.mu.Lock()

	// 获取当前房间中的不同用户数
	differentUserCount := len(room.userConnCount)

	// 检查当前用户是否已经在房间中
	_, userExists := room.userConnCount[connectMsg.UserID]

	// 如果房间已满（已有2个不同用户）且当前用户不在房间中，则拒绝
	if differentUserCount >= maxUsersPerRoom && !userExists {
		room.mu.Unlock()
		sendError(ws, "房间已满（最多2人）")
		return
	}

	// 为新用户分配玩家编号
	var playerNumber int
	if !userExists {
		playerNumber = len(room.allowedUsers)
		room.playerNumbers[connectMsg.UserID] = playerNumber
	} else {
		// 已存在用户，获取已有编号
		playerNumber = room.playerNumbers[connectMsg.UserID]
	}

	// 创建客户端
	client := &Client{
		conn:         ws,
		username:     connectMsg.Username,
		userID:       connectMsg.UserID,
		color:        generateColor(connectMsg.UserID),
		roomID:       connectMsg.RoomID,
		playerNumber: playerNumber,
		send:         make(chan []byte, 256),
		isHome:       true,
	}

	// 添加到房间
	room.clients[client] = true

	// 更新用户连接数
	if _, exists := room.userConnCount[connectMsg.UserID]; !exists {
		// 新用户加入，添加到允许用户列表
		if len(room.allowedUsers) < maxUsersPerRoom {
			room.allowedUsers[connectMsg.UserID] = true
		}
	}
	room.userConnCount[connectMsg.UserID]++

	room.mu.Unlock()

	log.Printf("%s (%s) 作为玩家 %d 加入了房间 %s", client.username, client.userID[:8], playerNumber, client.roomID)

	// 启动客户端的写入goroutine
	go client.writePump()

	// 发送玩家编号给客户端
	sendPlayerNumber(client, playerNumber)

	// 初始化游戏状态（如果房间刚创建）
	room.mu.Lock()
	if len(room.clients) == 1 {
		initializeGame(room)
	}
	room.mu.Unlock()

	// 发送当前游戏状态
	sendGameState(client, room)

	// 先发送历史消息给新用户
	sendHistoryToClient(client, room)

	// 发送欢迎消息（系统消息）
	sendWelcomeMessage(client, room)

	// 发送当前房间用户列表
	broadcastUserList(room)

	// 广播房间列表更新
	broadcastRoomList()

	// 读取消息
	client.readPump(room)
}

func (c *Client) readPump(room *Room) {
	defer func() {
		// 客户端断开连接
		room.mu.Lock()
		delete(room.clients, c)

		// 减少用户连接数
		if count, exists := room.userConnCount[c.userID]; exists {
			if count <= 1 {
				// 该用户的最后一个连接，从计数中删除
				delete(room.userConnCount, c.userID)
				delete(room.allowedUsers, c.userID)
				delete(room.playerNumbers, c.userID)
			} else {
				room.userConnCount[c.userID]--
			}
		}

		// 检查房间是否为空
		roomEmpty := len(room.clients) == 0
		room.mu.Unlock()

		// 发送离开消息（系统消息）
		sendLeaveMessage(c, room)

		// 更新用户列表
		broadcastUserList(room)

		// 如果房间空了，关闭房间
		if roomEmpty {
			roomsMu.Lock()
			delete(rooms, room.ID)
			roomsMu.Unlock()
			log.Printf("房间 %s 已关闭（无人）", room.ID)
		}

		// 广播房间列表更新
		broadcastRoomList()

		// 关闭发送通道和连接
		close(c.send)
		c.conn.Close()

		log.Printf("%s (%s) 离开了房间 %s", c.username, c.userID[:8], c.roomID)
	}()

	for {
		_, msgBytes, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("读取错误: %v", err)
			}
			break
		}

		var msg struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		}

		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			log.Printf("解析消息失败: %v", err)
			continue
		}

		switch msg.Type {
		case "message":
			handleMessage(c, room, msg.Content)
		case "rename":
			handleRename(c, room, msg.Content)
		case "gameState":
			handleGame(room, msg.Content)
		default:
		}
	}
}

func (c *Client) writePump() {
	defer c.conn.Close()

	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				log.Printf("写入消息失败: %v", err)
				return
			}
		}
	}
}

func handleMessage(client *Client, room *Room, content string) {
	msg := Message{
		Type:         "message",
		UserID:       client.userID,
		Username:     client.username,
		PlayerNumber: client.playerNumber,
		Content:      content,
		Time:         time.Now(),
		Color:        client.color,
		RoomID:       client.roomID,
	}

	// 保存到历史消息
	saveMessage(room, msg)

	// 广播给房间内所有用户
	broadcastToRoom(room, msg)
}

func handleRename(client *Client, room *Room, newName string) {
	oldName := client.username
	client.username = newName
	client.color = generateColor(client.userID)

	// 创建改名消息（系统消息）
	msg := Message{
		Type:     "system",
		UserID:   client.userID,
		Username: client.username,
		Content:  oldName + " 改名为 " + client.username,
		Time:     time.Now(),
		Color:    client.color,
		RoomID:   client.roomID,
	}

	// 广播给房间内所有用户
	broadcastToRoom(room, msg)
	broadcastUserList(room)
}

func sendWinMessage(client *Client, room *Room) {
	msg := Message{
		Type:     "system",
		UserID:   client.userID,
		Username: client.username,
		Content:  fmt.Sprintf("👋 %s 胡了 🎇 o(^▽^)o", client.username),
		Time:     time.Now(),
		Color:    client.color,
		RoomID:   client.roomID,
	}

	broadcastToRoom(room, msg)
}

func sendWelcomeMessage(client *Client, room *Room) {
	room.mu.RLock()
	userCount := len(room.userConnCount)
	room.mu.RUnlock()

	msg := Message{
		Type:     "system",
		UserID:   client.userID,
		Username: client.username,
		Content:  fmt.Sprintf("👋 用户 #%d %s 加入了房间 (%d/2)", client.playerNumber, client.username, userCount),
		Time:     time.Now(),
		Color:    client.color,
		RoomID:   client.roomID,
	}

	broadcastToRoom(room, msg)
}

func sendLeaveMessage(client *Client, room *Room) {
	room.mu.RLock()
	userCount := len(room.userConnCount)
	room.mu.RUnlock()

	msg := Message{
		Type:     "system",
		UserID:   client.userID,
		Username: client.username,
		Content:  fmt.Sprintf("🚪 用户 #%d %s 离开了房间 (%d/2)", client.playerNumber, client.username, userCount-1),
		Time:     time.Now(),
		Color:    client.color,
		RoomID:   client.roomID,
	}

	broadcastToRoom(room, msg)
}

func sendError(ws *websocket.Conn, message string) {
	msg := Message{
		Type:    "error",
		Content: message,
		Time:    time.Now(),
	}

	msgBytes, _ := json.Marshal(msg)
	ws.WriteMessage(websocket.TextMessage, msgBytes)
	ws.Close()
}

func sendPlayerNumber(client *Client, playerNumber int) {
	msg := Message{
		Type:         "playerNumber",
		PlayerNumber: playerNumber,
		Time:         time.Now(),
	}

	msgBytes, _ := json.Marshal(msg)
	select {
	case client.send <- msgBytes:
	default:
		client.conn.Close()
	}
}

func handleGame(room *Room, content string) {
	room.mu.Lock()

	msg := Message{
		Type:    "gameState",
		Content: content,
		Time:    time.Now(),
	}

	msgBytes, _ := json.Marshal(msg)
	// 发送给房间内所有用户
	for client := range room.clients {
		select {
		case client.send <- msgBytes:
		default:
			close(client.send)
			delete(room.clients, client)
			client.conn.Close()
		}
	}

	var gameState GameState
	err := json.Unmarshal([]byte(content), &gameState)
	if err != nil {
		log.Printf("解析游戏状态失败: %v", err)
		return
	}

	room.mu.Unlock()
	if gameState.GameAction == `胡` {
		var c *Client
		for c = range room.clients {
			if c.playerNumber == room.CurrentTurn {
				sendWinMessage(c, room)
			}
		}
		initializeGame(room)
		for c = range room.clients {
			sendGameState(c, room)
		}

	}
}

func initializeGame(room *Room) {
	rand.Seed(time.Now().UnixNano())

	faces := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 31, 33, 35, 37, 39, 41, 43}
	deck := make([]int, 0, 64)
	for i := 0; i < 4; i++ {
		deck = append(deck, faces...)
	}

	// 洗牌
	rand.Shuffle(len(deck), func(i, j int) {
		deck[i], deck[j] = deck[j], deck[i]
	})

	// 发牌
	playerA := make([]int, 13)
	copy(playerA, deck[:13])
	playerB := make([]int, 13)
	copy(playerB, deck[13:26])
	remaining := deck[26:]
	sort.Ints(playerA)
	sort.Ints(playerB)

	room.Deck = remaining
	room.GameState.CurrentTurn = rand.Intn(2)
	room.GameState.Hands = playerA
	room.GameState.Hands1 = playerB
	if room.GameState.CurrentTurn == 0 {
		room.GameState.Hands = append(room.GameState.Hands, room.Deck[0])
		room.Deck = room.Deck[1:]
	} else {
		room.GameState.Hands1 = append(room.GameState.Hands1, room.Deck[0])
		room.Deck = room.Deck[1:]
	}
	room.GameState.LittleTurn = 1
	room.GameState.GameAction = ``
	room.GameState.Discard = make([]int, 0)
	room.GameState.Extra = make([]int, 0)
	room.GameState.Extra1 = make([]int, 0)
	room.IsNew = true
}

func sendGameState(client *Client, room *Room) {
	room.mu.RLock()
	defer room.mu.RUnlock()
	// 创建游戏状态
	gameState := room.GameState

	gameStateJSON, err := json.Marshal(gameState)
	if err != nil {
		log.Printf("序列化游戏状态失败: %v", err)
		return
	}

	msg := Message{
		Type:    "gameState",
		Content: string(gameStateJSON),
		Time:    time.Now(),
	}

	msgBytes, _ := json.Marshal(msg)

	// 发送给房间内所有用户
	select {
	case client.send <- msgBytes:
	default:
		close(client.send)
		delete(room.clients, client)
		client.conn.Close()
	}
}

func saveMessage(room *Room, msg Message) {
	room.mu.Lock()
	defer room.mu.Unlock()

	// 只保存用户消息
	if msg.Type == "message" {
		room.messages = append(room.messages, msg)

		// 如果超过最大历史记录，删除最早的消息
		if len(room.messages) > maxHistory {
			room.messages = room.messages[1:]
		}
	}
}

func sendHistoryToClient(client *Client, room *Room) {
	room.mu.RLock()
	defer room.mu.RUnlock()

	if len(room.messages) > 0 {
		historyMsg := Message{
			Type:    "history",
			Time:    time.Now(),
			Content: "",
		}

		historyJSON, err := json.Marshal(room.messages)
		if err == nil {
			historyMsg.Content = string(historyJSON)
			msgBytes, _ := json.Marshal(historyMsg)

			select {
			case client.send <- msgBytes:
			default:
				client.conn.Close()
			}
		}
	}
}

func broadcastUserList(room *Room) {
	room.mu.RLock()
	userList := make([]map[string]interface{}, 0, len(room.clients))

	// 统计不同的用户
	for userID := range room.allowedUsers {
		// 找到该用户的一个客户端来获取信息
		for client := range room.clients {
			if client.userID == userID {
				userList = append(userList, map[string]interface{}{
					"userID":       client.userID,
					"username":     client.username,
					"color":        client.color,
					"playerNumber": client.playerNumber,
				})
				break
			}
		}
	}
	room.mu.RUnlock()

	userListJSON, _ := json.Marshal(userList)

	msg := Message{
		Type:    "userlist",
		Content: string(userListJSON),
		Time:    time.Now(),
	}

	broadcastToRoom(room, msg)
}

func broadcastToRoom(room *Room, msg Message) {
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("序列化消息失败: %v", err)
		return
	}

	room.mu.RLock()
	defer room.mu.RUnlock()

	for client := range room.clients {
		select {
		case client.send <- msgBytes:
		default:
			close(client.send)
			delete(room.clients, client)
			client.conn.Close()
		}
	}
}

// 获取房间列表信息
func getRoomList() []map[string]interface{} {
	roomsMu.RLock()
	defer roomsMu.RUnlock()

	roomList := make([]map[string]interface{}, 0, len(rooms))
	for _, room := range rooms {
		room.mu.RLock()
		userCount := len(room.userConnCount)
		availableSlots := maxUsersPerRoom - userCount
		room.mu.RUnlock()

		roomList = append(roomList, map[string]interface{}{
			"id":             room.ID,
			"name":           room.Name,
			"userCount":      userCount,
			"availableSlots": availableSlots,
			"maxUsers":       maxUsersPerRoom,
			"createdAt":      room.createdAt.Format("15:04"),
			"createdAgo":     formatTimeAgo(room.createdAt),
		})
	}

	return roomList
}

// 格式化时间差
func formatTimeAgo(t time.Time) string {
	duration := time.Since(t)

	if duration < time.Minute {
		return "刚刚"
	} else if duration < time.Hour {
		return fmt.Sprintf("%d分钟前", int(duration.Minutes()))
	} else if duration < 24*time.Hour {
		return fmt.Sprintf("%d小时前", int(duration.Hours()))
	}
	return fmt.Sprintf("%d天前", int(duration.Hours()/24))
}

// 发送房间列表给特定客户端
func sendRoomListToClient(client *Client) {
	roomList := getRoomList()

	msg := Message{
		Type:    "roomlist",
		Time:    time.Now(),
		Content: "",
	}

	roomListJSON, err := json.Marshal(roomList)
	if err == nil {
		msg.Content = string(roomListJSON)
		msgBytes, _ := json.Marshal(msg)

		select {
		case client.send <- msgBytes:
		default:
			client.conn.Close()
		}
	}
}

// 广播房间列表给所有主页客户端
func broadcastRoomList() {
	roomList := getRoomList()

	msg := Message{
		Type:    "roomlist",
		Time:    time.Now(),
		Content: "",
	}

	roomListJSON, err := json.Marshal(roomList)
	if err == nil {
		msg.Content = string(roomListJSON)
		msgBytes, _ := json.Marshal(msg)

		homeClientsMu.RLock()
		defer homeClientsMu.RUnlock()

		for client := range homeClients {
			select {
			case client.send <- msgBytes:
			default:
				close(client.send)
				delete(homeClients, client)
				client.conn.Close()
			}
		}
	}
}

// 生成随机房间号
func generateRoomID() string {
	roomsMu.RLock()
	defer roomsMu.RUnlock()

	// 尝试生成随机房间号
	for i := 0; i < 100; i++ {
		roomID := strconv.Itoa(100 + rand.Intn(900)) // 100-999
		if _, exists := rooms[roomID]; !exists {
			return roomID
		}
	}

	// 如果随机生成失败，遍历寻找可用房间号
	for id := 100; id <= 999; id++ {
		roomID := strconv.Itoa(id)
		if _, exists := rooms[roomID]; !exists {
			return roomID
		}
	}

	return "" // 没有可用房间号
}

// 生成随机房间名
func generateRoomName() string {
	return roomNames[rand.Intn(len(roomNames))]
}

// 生成用户颜色
func generateColor(userID string) string {
	colors := []string{
		"#FF6B6B", "#4ECDC4", "#FFD166", "#06D6A0",
		"#118AB2", "#073B4C", "#EF476F", "#7209B7",
		"#3A86FF", "#FB5607", "#8338EC", "#FF006E",
	}

	sum := 0
	for _, c := range userID {
		sum += int(c)
	}
	return colors[sum%len(colors)]
}

// API: 创建房间
func createRoomHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 生成房间ID
	roomID := generateRoomID()
	if roomID == "" {
		http.Error(w, "房间已满", http.StatusServiceUnavailable)
		return
	}

	// 创建新房间
	room := &Room{
		ID:            roomID,
		Name:          generateRoomName(),
		clients:       make(map[*Client]bool),
		allowedUsers:  make(map[string]bool),
		userConnCount: make(map[string]int),
		playerNumbers: make(map[string]int),
		messages:      make([]Message, 0),
		createdAt:     time.Now(),
	}

	roomsMu.Lock()
	rooms[roomID] = room
	roomsMu.Unlock()

	log.Printf("新房间创建: %s (%s)", roomID, room.Name)

	// 广播房间列表更新
	broadcastRoomList()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "success",
		"roomID":   roomID,
		"roomName": room.Name,
	})
}

// API: 获取房间列表
func listRoomsHandler(w http.ResponseWriter, r *http.Request) {
	roomList := getRoomList()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(roomList)
}
