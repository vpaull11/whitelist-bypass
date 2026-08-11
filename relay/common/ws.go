package common

import (
	"time"

	"github.com/gorilla/websocket"
)

func CloseWS(ws *websocket.Conn) {
	if ws == nil {
		return
	}
	// Send close frame with 1s deadline; ignore errors (connection may be dead).
	_ = ws.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second),
	)
	_ = ws.Close()
}
