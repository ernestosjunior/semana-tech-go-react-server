package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/ernestosjunior/semana-tech-go-react-server/internal/store/pgstore"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
)

type apiHandler struct {
	q           *pgstore.Queries
	r           *chi.Mux
	upgrader    websocket.Upgrader
	subscribers map[string]map[*websocket.Conn]context.CancelFunc
	mu          *sync.Mutex
}

const (
	MessageKindMessageCreated       = "message_created"
	MessageKindMessageReacted       = "message_reacted"
	MessageKindMessageRemoveReacted = "message_remove_reacted"
	MessageKingMessageAnswered      = "message_answered"
)

type MessageMessageCreated struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

type MessageID struct {
	ID string `json:"id"`
}
type MessageReaction struct {
	ID    string `json:"id"`
	Count int64  `json:"count"`
}

type Message struct {
	Kind   string `json:"kind"`
	Value  any    `json:"value"`
	RoomID string `json:"-"`
}

func (h apiHandler) NotifyClients(msg Message) {
	h.mu.Lock()
	defer h.mu.Unlock()

	subscriber, ok := h.subscribers[msg.RoomID]

	if !ok || len(subscriber) == 0 {
		return
	}

	for conn, cancel := range subscriber {
		if err := conn.WriteJSON(msg); err != nil {
			slog.Error("failed to send message to client", err)
			cancel()
		}
	}
}

func (h apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.r.ServeHTTP(w, r)
}

func NewHandler(q *pgstore.Queries) http.Handler {
	a := apiHandler{
		q: q,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		subscribers: make(map[string]map[*websocket.Conn]context.CancelFunc),
		mu:          &sync.Mutex{},
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, middleware.Logger)

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Get("/subscribe/{room_id}", a.handleSubscribe)
	r.Route("/api", func(r chi.Router) {
		r.Route("/rooms", func(r chi.Router) {
			r.Post("/", a.handleCreateRoom)
			r.Get("/", a.handleGetRooms)
			r.Route("/{room_id}", func(r chi.Router) {
				r.Get("/", a.handleGetRoom)
				r.Route("/messages", func(r chi.Router) {
					r.Post("/", a.handleCreateRoomMessage)
					r.Get("/", a.handleGetRoomMessages)
					r.Route("/{message_id}", func(r chi.Router) {
						r.Get("/", a.handleGetRoomMessage)
						r.Patch("/react", a.handleReactToMessage)
						r.Delete("/react", a.handleRemoveReactFromMessage)
						r.Patch("/answer", a.handleMarkAsAnswered)
					})
				})
			})

		})
	})

	a.r = r

	return a
}

func (h apiHandler) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	rawRoomId := chi.URLParam(r, "room_id")
	roomId, err := uuid.Parse(rawRoomId)
	if err != nil {
		http.Error(w, "invalid room id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomId)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	c, err := h.upgrader.Upgrade(w, r, nil)

	if err != nil {
		slog.Warn("error upgrading connection", err)
		http.Error(w, "failed to upgrade to ws connection", http.StatusBadRequest)
		return
	}

	defer c.Close()

	ctx, cancel := context.WithCancel(r.Context())

	h.mu.Lock()

	if _, ok := h.subscribers[rawRoomId]; !ok {
		h.subscribers[rawRoomId] = make(map[*websocket.Conn]context.CancelFunc)
	}
	slog.Info("subscribed to room", rawRoomId, r.RemoteAddr)
	h.subscribers[rawRoomId][c] = cancel

	h.mu.Unlock()

	<-ctx.Done()

	h.mu.Lock()

	delete(h.subscribers[rawRoomId], c)

	h.mu.Unlock()
}

func (h apiHandler) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	type _body struct {
		Theme string `json:"theme"`
	}

	var body _body

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	roomId, err := h.q.CreateRoom(r.Context(), body.Theme)

	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		slog.Warn("failed to create room", "error", err)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	SendJSON(w, response{ID: roomId.String()})
}

func (h apiHandler) handleGetRoom(w http.ResponseWriter, r *http.Request) {
	room, _, _, ok := h.ReadRoom(w, r)

	if !ok {
		return
	}

	type response struct {
		ID    string `json:"id"`
		Theme string `json:"theme"`
	}

	SendJSON(w, response{ID: room.ID.String(), Theme: room.Theme})
}

func (h apiHandler) handleGetRooms(w http.ResponseWriter, r *http.Request) {
	rooms, err := h.q.GetRooms(r.Context())

	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		slog.Error("failed to get rooms", "error", err)
		return
	}

	if rooms == nil {
		rooms = []pgstore.Room{}
	}

	SendJSON(w, rooms)
}

func (h apiHandler) handleGetRoomMessages(w http.ResponseWriter, r *http.Request) {
	_, _, roomId, ok := h.ReadRoom(w, r)

	if !ok {
		return
	}

	messages, err := h.q.GetRoomMessages(r.Context(), roomId)

	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		slog.Warn("failed to get messages", "error", err)
		return
	}

	if messages == nil {
		messages = []pgstore.Message{}
	}

	SendJSON(w, messages)
}

func (h apiHandler) handleCreateRoomMessage(w http.ResponseWriter, r *http.Request) {
	_, rawRoomId, roomId, ok := h.ReadRoom(w, r)

	if !ok {
		return
	}

	type _body struct {
		Message string `json:"message"`
	}

	var body _body

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	messageId, err := h.q.InsertMessage(r.Context(), pgstore.InsertMessageParams{
		RoomID:  roomId,
		Message: body.Message,
	})

	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		slog.Warn("failed to create message", "error", err)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	SendJSON(w, response{ID: messageId.String()})

	go h.NotifyClients(Message{
		Kind:   MessageKindMessageCreated,
		RoomID: rawRoomId,
		Value:  MessageMessageCreated{ID: messageId.String(), Message: body.Message},
	})
}

func (h apiHandler) handleGetRoomMessage(w http.ResponseWriter, r *http.Request) {
	_, _, _, ok := h.ReadRoom(w, r)

	if !ok {
		return
	}

	rawMessageId := chi.URLParam(r, "message_id")

	messageId, err := uuid.Parse(rawMessageId)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	message, err := h.q.GetRoomMessage(r.Context(), messageId)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	}

	SendJSON(w, response{ID: message.ID.String(), Message: message.Message})
}

func (h apiHandler) handleReactToMessage(w http.ResponseWriter, r *http.Request) {
	_, rawRoomId, _, ok := h.ReadRoom(w, r)

	if !ok {
		return
	}

	rawMessageId := chi.URLParam(r, "message_id")

	messageId, err := uuid.Parse(rawMessageId)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoomMessage(r.Context(), messageId)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	count, err := h.q.ReactToMessage(r.Context(), messageId)

	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		slog.Warn("failed to react to message", "error", err)
		return
	}

	type response struct {
		Count int64 `json:"count"`
	}

	SendJSON(w, response{Count: count})

	go h.NotifyClients(Message{
		Kind:   MessageKindMessageReacted,
		RoomID: rawRoomId,
		Value: MessageReaction{
			Count: count,
			ID:    messageId.String(),
		},
	})
}

func (h apiHandler) handleRemoveReactFromMessage(w http.ResponseWriter, r *http.Request) {
	_, rawRoomId, _, ok := h.ReadRoom(w, r)

	if !ok {
		return
	}

	rawMessageId := chi.URLParam(r, "message_id")

	messageId, err := uuid.Parse(rawMessageId)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoomMessage(r.Context(), messageId)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	count, err := h.q.RemoveReactionFromMessage(r.Context(), messageId)

	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		slog.Warn("failed to remove reaction from message", "error", err)
		return
	}

	type response struct {
		Count int64 `json:"count"`
	}

	SendJSON(w, response{Count: count})

	go h.NotifyClients(Message{
		Kind:   MessageKindMessageRemoveReacted,
		RoomID: rawRoomId,
		Value: MessageReaction{
			Count: count,
			ID:    messageId.String(),
		},
	})
}

func (h apiHandler) handleMarkAsAnswered(w http.ResponseWriter, r *http.Request) {
	_, _, roomId, ok := h.ReadRoom(w, r)

	if !ok {
		return
	}

	rawMessageId := chi.URLParam(r, "message_id")

	messageId, err := uuid.Parse(rawMessageId)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoomMessage(r.Context(), messageId)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	_, err = h.q.MarkMessageAsAnswered(r.Context(), messageId)

	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		slog.Warn("failed to mark message as answered", "error", err)
		return
	}

	w.WriteHeader(http.StatusOK)

	go h.NotifyClients(Message{
		Kind:   MessageKingMessageAnswered,
		RoomID: roomId.String(),
		Value: MessageID{
			ID: messageId.String(),
		},
	})
}
