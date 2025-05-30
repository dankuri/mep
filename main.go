package main

import (
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/gorilla/websocket"
	"github.com/oklog/ulid/v2"
)

type RoomID = ulid.ULID

type Member struct {
	Name string
	Conn *websocket.Conn
}

type Room struct {
	ID          RoomID
	CreatorName string
	members     map[string]*Member
	membersLock *sync.RWMutex
}

// AddMember add the member to the room if he doesn't exist.
// Returns false if member already exists.
func (r *Room) AddMember(username string, conn *websocket.Conn) bool {
	r.membersLock.Lock()
	defer r.membersLock.Unlock()

	if _, ok := r.members[username]; ok {
		return false
	}

	r.members[username] = &Member{Name: username, Conn: conn}
	return true
}

func (r *Room) GetOtherMembers(username string) []*Member {
	r.membersLock.RLock()
	defer r.membersLock.RUnlock()

	filteredMembers := make([]*Member, 0, max(len(r.members)-1, 0))
	for id, member := range r.members {
		if id != username {
			filteredMembers = append(filteredMembers, member)
		}
	}

	return filteredMembers
}

func main() {
	upgrader := new(websocket.Upgrader)

	rooms := map[RoomID]*Room{}
	roomsLock := new(sync.RWMutex)

	http.HandleFunc("POST /room", func(w http.ResponseWriter, r *http.Request) {
		logger := slog.With("path", r.URL.Path, "method", r.Method)

		var data struct {
			Username string `json:"username"`
		}

		err := json.NewDecoder(r.Body).Decode(&data)
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if data.Username == "" {
			http.Error(w, "username can't be empty", http.StatusBadRequest)
		}

		roomID := ulid.Make()

		roomsLock.Lock()
		rooms[roomID] = &Room{
			ID:          roomID,
			CreatorName: data.Username,
			members:     map[string]*Member{},
			membersLock: &sync.RWMutex{},
		}
		roomsLock.Unlock()

		err = json.NewEncoder(w).Encode(map[string]any{"room_id": roomID})
		if err != nil {
			logger.Error("json.Encode", "err", err)
		}
	})

	http.HandleFunc("GET /room/{room_id}", func(w http.ResponseWriter, r *http.Request) {
		roomIDStr := r.PathValue("room_id")
		if roomIDStr == "" {
			http.Error(w, "room_id can't be empty", http.StatusBadRequest)
			return
		}
		roomID, err := ulid.Parse(roomIDStr)
		if err != nil {
			http.Error(w, "room_id is not valid ULID", http.StatusBadRequest)
			return
		}

		roomsLock.RLock()
		room, found := rooms[roomID]
		roomsLock.RUnlock()
		if !found {
			http.Error(w, fmt.Sprintf("room %s not found", roomID), http.StatusNotFound)
			return
		}

		logger := slog.With("room_id", roomID)

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			slog.Error("upgrader.Upgrade", "err", err)
			return
		}
		defer conn.Close()

		mt, msg, err := conn.ReadMessage()
		if err != nil {
			slog.Error("conn.ReadFirstMessage", "err", err)
			return
		}
		if mt != websocket.TextMessage {
			http.Error(w, "invalid first message type", http.StatusBadRequest)
			return
		}

		username := string(msg)
		logger = logger.With("username", username)

		if !room.AddMember(username, conn) {
			http.Error(w, "user already in room", http.StatusBadRequest)
			return
		}
		defer func() {
			roomsLock.Lock()
			defer roomsLock.Unlock()
			delete(rooms[roomID].members, username)
		}()

		// TODO: this should be second message from user if he's the Room Creator
		files, err := os.ReadDir(".")
		if err != nil {
			logger.Error("os.ReadDir", "err", err)
			return
		}
		fileList := make([]string, 0, len(files))
		for _, file := range files {
			entry := file.Name()
			if file.IsDir() {
				entry += "/"
			}
			fileList = append(fileList, entry)
		}

		// TODO: this should send the filelist only to other members, if they're already connected
		// TODO: also store fileList in the room
		err = conn.WriteMessage(websocket.TextMessage, []byte(strings.Join(fileList, "\n")))
		if err != nil {
			logger.Error("conn.WriteMessage(fileList)", "err", err)
			return
		}

		for {
			mt, message, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					logger.Warn("connection closed", "err", err)
					break
				}
				logger.Error("conn.ReadMessage", "err", err)
				break
			}
			logger.Info("incoming", "msg", message)

			for _, member := range room.GetOtherMembers(username) {
				err = member.Conn.WriteMessage(mt, message)
				if err != nil {
					logger.Error("otherMember.WriteMessage", "err", err, "other_member_name", member.Name)
					break
				}
			}
			if err != nil {
				break
			}

			err = conn.WriteMessage(websocket.TextMessage, []byte("ok"))
			if err != nil {
				logger.Error("conn.WriteMessage", "err", err)
				break
			}
		}
	})

	addr := "localhost:8080"

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Fatal(http.ListenAndServe(addr, nil))
	}()
	slog.Info("server listening", "addr", addr)

	<-stop
	slog.Info("stopping application")
}
