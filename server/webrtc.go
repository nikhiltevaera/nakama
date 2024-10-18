package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/pion/webrtc/v3"
	"go.uber.org/zap"
)

// RpcFunction defines the signature for an RPC function.
type RpcFunction func(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, payload string) (string, error)

// PayloadWebRTC defines the structure of incoming WebRTC data channel messages.
type PayloadWebRTC struct {
	RPC     string          `json:"rpc"`
	Payload json.RawMessage `json:"data"`
}

// WebRTCServer manages WebRTC connections and data channels.
type WebRTCServer struct {
	Conn     *webrtc.PeerConnection
	Log      *zap.Logger
	DataChs  map[string]*webrtc.DataChannel
	mu       sync.Mutex
	Handlers map[string]func([]byte)
}

// WebRTCManager manages multiple WebRTC connections.
type WebRTCManager struct {
	Servers map[string]*WebRTCServer
	mu      sync.Mutex
}

var webRTCServerInstance *WebRTCServer

// NewWebRTCManager creates a new WebRTC manager.
func NewWebRTCManager() *WebRTCManager {
	return &WebRTCManager{
		Servers: make(map[string]*WebRTCServer),
	}
}

func WebRTCServerConfigure(logger *zap.Logger) (*WebRTCServer, error) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		logger.Error("Failed to create peer connection: %v", zap.String("err", err.Error()))
		return nil, err
	}

	server := &WebRTCServer{
		Conn:     pc,
		Log:      logger,
		DataChs:  make(map[string]*webrtc.DataChannel),
		Handlers: make(map[string]func([]byte)),
	}

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		logger.Info("ICE Connection State has changed: %s", zap.String("state", string(state)))
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		logger.Info("Data channel received: %s", zap.String("channel", dc.Label()))
		server.mu.Lock()
		defer server.mu.Unlock()
		server.DataChs[dc.Label()] = dc
		server.handleDataChannel(dc)
	})

	return server, nil
}

// SendResponse sends a response over the WebRTC data channel.
func (server *WebRTCServer) SendResponse(channelLabel string, response string) {
	server.mu.Lock()
	defer server.mu.Unlock()

	// Check if the data channel with the given label exists
	dataCh, exists := server.DataChs[channelLabel]
	if !exists {
		server.Log.Error("Data channel not found", zap.String("label", channelLabel))
		return
	}

	// Check if the data channel is open before sending the response
	if dataCh.ReadyState() != webrtc.DataChannelStateOpen {
		server.Log.Error("Data channel is not open", zap.String("label", channelLabel))
		return
	}

	// Send the response over the selected data channel
	err := dataCh.SendText(response)
	if err != nil {
		server.Log.Error("Error occurred during sending data", zap.String("label", channelLabel), zap.String("err", err.Error()))
		return
	}

	server.Log.Info("Sent response", zap.String("label", channelLabel), zap.String("response", response))
}

// SendErrorResponse sends an error response to the client on the specified data channel.
func (server *WebRTCServer) SendErrorResponse(channelLabel string, errorMsg string) {
	// Create the error response as a JSON object
	errorResponse := map[string]string{"error": errorMsg}

	// Marshal the error response into a JSON byte array
	responseBytes, err := json.Marshal(errorResponse)
	if err != nil {
		server.Log.Error("Failed to marshal error response", zap.String("err", err.Error()))
		return
	}

	// Send the error response on the specified data channel
	server.SendResponse(channelLabel, string(responseBytes))
}

// handleWebSocket handles WebSocket connections for individual clients.
func handleWebSocket(ws *websocket.Conn, server *WebRTCServer, logger *zap.Logger) {
	if server == nil {
		logger.Error("WebRTC server is nil")
		return
	}
	ctx := context.Background()

	for {
		var message []byte
		_, message, err := ws.Read(ctx)
		if err != nil {
			if _, ok := err.(*websocket.CloseError); ok {
				logger.Info("WebSocket connection closed")
				break
			}
			logger.Error("Error reading WebSocket message: %v", zap.Error(err))
			break
		}

		logger.Info("Received WebSocket message: %s", zap.String("message", string(message)))

		var data map[string]interface{}
		if err := json.Unmarshal(message, &data); err != nil {
			logger.Error("Error unmarshalling WebSocket message: %v", zap.String("err", err.Error()))
			continue
		}

		switch data["type"] {
		case "offer":
			offerData, ok := data["offer"].(map[string]interface{})
			if !ok {
				logger.Error("Invalid offer data")
				continue
			}

			offer := webrtc.SessionDescription{}
			switch offerData["type"].(string) {
			case "offer":
				offer.Type = webrtc.SDPTypeOffer
			case "answer":
				offer.Type = webrtc.SDPTypeAnswer
			case "pranswer":
				offer.Type = webrtc.SDPTypePranswer
			default:
				logger.Error("Invalid SDP type")
				continue
			}

			offer.SDP = offerData["sdp"].(string)

			if err := server.Conn.SetRemoteDescription(offer); err != nil {
				logger.Error("Error setting remote description: %v", zap.String("err", err.Error()))
				continue
			}

			answer, err := server.Conn.CreateAnswer(nil)
			if err != nil {
				logger.Error("Error creating answer: %v", zap.String("err", err.Error()))
				continue
			}

			if err := server.Conn.SetLocalDescription(answer); err != nil {
				logger.Error("Error setting local description: %v", zap.String("err", err.Error()))
				continue
			}

			answerData, err := json.Marshal(map[string]interface{}{
				"type":   "answer",
				"answer": server.Conn.LocalDescription(),
			})
			if err != nil {
				logger.Error("Error marshalling answer data: %v", zap.String("err", err.Error()))
				continue
			}

			if err := ws.Write(ctx, websocket.MessageBinary, answerData); err != nil {
				logger.Error("Error writing answer data to WebSocket: %v", zap.String("err", err.Error()))
				continue
			}

		case "iceCandidate":
			// Handle ICE candidates
			candidateData, ok := data["candidate"].(map[string]interface{})
			if !ok {
				logger.Error("Invalid ICE candidate data")
				continue
			}

			sdpMid := candidateData["sdpMid"].(string)
			sdpMLineIndex := uint16(candidateData["sdpMLineIndex"].(float64))
			candidate := webrtc.ICECandidateInit{
				Candidate:     candidateData["candidate"].(string),
				SDPMid:        &sdpMid,
				SDPMLineIndex: &sdpMLineIndex,
			}

			if err := server.Conn.AddICECandidate(candidate); err != nil {
				logger.Error("Error adding ICE candidate: %v", zap.String("err", err.Error()))
				continue
			}
		}
	}
}

// setupCORS sets up Cross-Origin Resource Sharing headers.
func setupCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

// setup data channel handlers
func (server *WebRTCServer) setupDataChannelHandlers() {
	// Handler for 'game' data channel
	server.Handlers["game"] = func(data []byte) {
		// Create an instance of PayloadWebRTC to hold the unmarshalled data
		var request PayloadWebRTC

		// Unmarshal the incoming JSON message into the PayloadWebRTC struct
		if err := json.Unmarshal(data, &request); err != nil {
			server.Log.Error("Error unmarshalling WebSocket message", zap.String("err", err.Error()))
			return // Exit the function if there is an error
		}

		// Logic for processing game messages
		server.Log.Info("Received game action:", zap.String("rpc", request.RPC))

		switch request.RPC {
		case "healthcheck":
			server.SendResponse("game", "Game started")
		default:
			server.Log.Warn("Unknown game RPC", zap.String("rpc", request.RPC))
		}
	}

	// Handler for 'chat' data channel
	server.Handlers["chat"] = func(data []byte) {
		// Logic for chat messages
		server.SendResponse("chat", "Chat message processed")
	}
}

// handleDataChannel handles the data channel events.
func (server *WebRTCServer) handleDataChannel(dc *webrtc.DataChannel) {
	server.Log.Info("New data channel created: %s", zap.String("label", dc.Label()))

	dc.OnOpen(func() {
		server.Log.Info("Data channel '%s' is open.", zap.String("label", dc.Label()))
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		server.Log.Info("Received message: %s", zap.String("data", string(msg.Data)))
		// Get the handler based on the data channel label
		server.mu.Lock()
		handler, exists := server.Handlers[dc.Label()]
		server.mu.Unlock()

		// If handler exists, process the message
		if exists {
			handler(msg.Data)
		} else {
			server.Log.Warn("No handler for data channel", zap.String("label", dc.Label()))
			server.SendErrorResponse(dc.Label(), "No handler available")
		}
	})

	dc.OnClose(func() {
		server.Log.Info("Data channel '%s' is closed.", zap.String("label", dc.Label()))
	})

	dc.OnError(func(err error) {
		server.Log.Error("Data channel error: %v", zap.String("err", err.Error()))
	})
}

func (server *WebRTCServer) Close() {
	server.mu.Lock()
	defer server.mu.Unlock()

	// Close the WebRTC peer connection.
	if server.Conn != nil {
		server.Conn.Close()
	}

	// Close the WebRTC data channel.
	for _, dc := range server.DataChs {
		dc.Close()
	}
}

func (mh *WebRTCServer) Stop() {
	mh.Close()
}

// InitModule initializes the Nakama module and starts the WebRTC server.
func StartWebRTCServer(ctx context.Context, logger *zap.Logger) *WebRTCServer {
	startTime := time.Now()

	manager := NewWebRTCManager()
	var server *WebRTCServer

	http.Handle("/ws", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setupCORS(w)

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			logger.Error("Failed to accept WebSocket connection: %v", zap.String("err", err.Error()))
			return
		}

		server, err := WebRTCServerConfigure(logger)
		if err != nil {
			logger.Error("Failed to start WebRTC server: %v", zap.String("err", err.Error()))
			return
		}
		//setup data channel Handlers
		server.setupDataChannelHandlers()

		// Unique ID for each WebRTC connection, could be something meaningful like session ID.
		clientID := r.RemoteAddr

		manager.mu.Lock()
		manager.Servers[clientID] = server
		manager.mu.Unlock()

		handleWebSocket(conn, server, logger)
		webRTCServerInstance = server

		// Cleanup after the connection is closed
		manager.mu.Lock()
		delete(manager.Servers, clientID)
		manager.mu.Unlock()

		logger.Info("Closed WebSocket connection with client: %s", zap.String("clientID", clientID))
	}))

	go func() {
		logger.Info("Starting WebSocket server on :9876")
		if err := http.ListenAndServe(":9876", nil); err != nil {
			logger.Error("Error starting WebSocket server: %v ", zap.String("error", err.Error()))
		}
	}()

	logger.Info("WebRTC server started in %s", zap.String("duration", time.Since(startTime).String()))
	return server
}

func GetWebRTCServerInstance() *WebRTCServer {
	return webRTCServerInstance
}
