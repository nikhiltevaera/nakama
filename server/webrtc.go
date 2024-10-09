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
	Conn   *webrtc.PeerConnection
	Log    *zap.Logger
	DataCh *webrtc.DataChannel
	mu     sync.Mutex
}

// WebRTCManager manages multiple WebRTC connections.
type WebRTCManager struct {
	Servers map[string]*WebRTCServer
	mu      sync.Mutex
}

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
		Conn: pc,
		Log:  logger,
	}

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		logger.Info("ICE Connection State has changed: %s", zap.String("state", string(state)))
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		logger.Info("Data channel received: %s", zap.String("channel", dc.Label()))
		server.mu.Lock()
		defer server.mu.Unlock()
		server.DataCh = dc
		server.handleDataChannel(server.DataCh)
	})

	return server, nil
}

// ProcessPacket handles incoming data from the WebRTC data channel.
func (server *WebRTCServer) ProcessPacket(data []byte) {
	var request PayloadWebRTC
	if err := json.Unmarshal(data, &request); err != nil {
		server.Log.Error("Failed to unmarshal data: %v", zap.String("err", err.Error()))
		return
	}

	server.SendResponse("Process Packet")
}

// SendResponse sends a response over the WebRTC data channel.
func (server *WebRTCServer) SendResponse(response string) {
	server.mu.Lock()
	defer server.mu.Unlock()

	if server.DataCh == nil {
		server.Log.Error("Data channel is not initialized")
		return
	}

	err := server.DataCh.Send([]byte(response))
	if err != nil {
		server.Log.Error("Error occurred during sending data: %v", zap.String("err", err.Error()))
		return
	}
	server.Log.Info("Sent response: %s", zap.String("response", response))
}

// SendErrorResponse sends an error response to the client.
func (server *WebRTCServer) SendErrorResponse(errorMsg string) {
	errorResponse := map[string]string{"error": errorMsg}
	responseBytes, _ := json.Marshal(errorResponse)
	server.SendResponse(string(responseBytes))
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

// handleDataChannel handles the data channel events.
func (server *WebRTCServer) handleDataChannel(dc *webrtc.DataChannel) {
	server.Log.Info("New data channel created: %s", zap.String("label", dc.Label()))

	dc.OnOpen(func() {
		server.Log.Info("Data channel '%s' is open.", zap.String("label", dc.Label()))
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		server.Log.Info("Received message: %s", zap.String("data", string(msg.Data)))
		server.ProcessPacket(msg.Data)
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
	if server.DataCh != nil {
		server.DataCh.Close()
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

		// Unique ID for each WebRTC connection, could be something meaningful like session ID.
		clientID := r.RemoteAddr

		manager.mu.Lock()
		manager.Servers[clientID] = server
		manager.mu.Unlock()

		handleWebSocket(conn, server, logger)

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
