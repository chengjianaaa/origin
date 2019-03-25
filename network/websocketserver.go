package network

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/duanhf2012/origin/service"
	"github.com/duanhf2012/origin/sysmodule"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/gotoxu/cors"
)

type IWebsocketServer interface {
	SendMsg(clientid uint64, messageType int, msg []byte) bool
	CreateClient(conn *websocket.Conn) *WSClient
	Disconnect(clientid uint64)
	ReleaseClient(pclient *WSClient)
}

type IMessageReceiver interface {
	initReciver(messageReciver IMessageReceiver, websocketServer IWebsocketServer)

	OnConnected(clientid uint64)
	OnDisconnect(clientid uint64, err error)
	OnRecvMsg(clientid uint64, msgtype int, data []byte)
	OnHandleHttp(w http.ResponseWriter, r *http.Request)
}

type Reciver struct {
	messageReciver     IMessageReceiver
	bEnableCompression bool
}

type BaseMessageReciver struct {
	messageReciver IMessageReceiver
	WsServer       IWebsocketServer
}

type WSClient struct {
	clientid  uint64
	conn      *websocket.Conn
	bwritemsg chan WSMessage
}

type WSMessage struct {
	msgtype   int
	bwritemsg []byte
}

type WebsocketServer struct {
	wsUri       string
	maxClientid uint64 //记录当前最新clientid
	mapClient   map[uint64]*WSClient
	locker      sync.Mutex

	port uint16

	httpserver *http.Server
	reciver    map[string]Reciver

	certfile string
	keyfile  string
	iswss    bool
}

func (slf *WebsocketServer) Init(port uint16) {

	slf.port = port
	slf.mapClient = make(map[uint64]*WSClient)
}

func (slf *WebsocketServer) CreateClient(conn *websocket.Conn) *WSClient {
	slf.locker.Lock()
	slf.maxClientid++
	clientid := slf.maxClientid
	pclient := &WSClient{clientid, conn, make(chan WSMessage, 1024)}
	slf.mapClient[pclient.clientid] = pclient
	slf.locker.Unlock()

	return pclient
}

func (slf *WebsocketServer) ReleaseClient(pclient *WSClient) {
	pclient.conn.Close()
	slf.locker.Lock()
	delete(slf.mapClient, pclient.clientid)
	slf.locker.Unlock()
	//关闭写管道
	close(pclient.bwritemsg)
}

func (slf *WebsocketServer) SetupReciver(pattern string, messageReciver IMessageReceiver, bEnableCompression bool) {
	messageReciver.initReciver(messageReciver, slf)

	if slf.reciver == nil {
		slf.reciver = make(map[string]Reciver)
	}
	slf.reciver[pattern] = Reciver{messageReciver, bEnableCompression}
}

func (slf *WebsocketServer) startListen() {
	listenPort := fmt.Sprintf(":%d", slf.port)

	slf.httpserver = &http.Server{
		Addr:           listenPort,
		Handler:        slf.initRouterHandler(),
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	var err error
	if slf.iswss == true {
		err = slf.httpserver.ListenAndServeTLS(slf.certfile, slf.keyfile)
	} else {
		err = slf.httpserver.ListenAndServe()
	}

	if err != nil {
		service.GetLogger().Printf(sysmodule.LEVER_FATAL, "http.ListenAndServe(%d, nil) error:%v\n", slf.port, err)
		os.Exit(1)
	}
}

func (slf *WSClient) startSendMsg() {
	for {
		msgbuf, ok := <-slf.bwritemsg
		if ok == false {
			break
		}

		err := slf.conn.WriteMessage(msgbuf.msgtype, msgbuf.bwritemsg)
		if err != nil {
			service.GetLogger().Printf(sysmodule.LEVER_INFO, "write client id %d is error :%v\n", slf.clientid, err)
			break
		}
	}
}

func (slf *WebsocketServer) Start() {

	go slf.startListen()
}

func (slf *WebsocketServer) SendMsg(clientid uint64, messageType int, msg []byte) bool {
	slf.locker.Lock()
	defer slf.locker.Unlock()
	value, ok := slf.mapClient[clientid]
	if ok == false {
		return false
	}

	value.bwritemsg <- WSMessage{messageType, msg}

	return true
}

func (slf *WebsocketServer) Disconnect(clientid uint64) {
	slf.locker.Lock()
	defer slf.locker.Unlock()
	value, ok := slf.mapClient[clientid]
	if ok == false {
		return
	}

	value.conn.Close()
}

func (slf *WebsocketServer) Stop() {
}

func (slf *BaseMessageReciver) startReadMsg(pclient *WSClient) {
	defer func() {
		if r := recover(); r != nil {
			var coreInfo string
			str, ok := r.(string)
			if ok {
				coreInfo = string(debug.Stack())
			} else {
				coreInfo = "Panic!"
			}

			coreInfo += "\n" + fmt.Sprintf("core information is %s\n", str)
			service.GetLogger().Printf(service.LEVER_FATAL, coreInfo)
			slf.messageReciver.OnDisconnect(pclient.clientid, errors.New("core dump"))
			slf.WsServer.ReleaseClient(pclient)
		}
	}()

	for {
		msgtype, message, err := pclient.conn.ReadMessage()
		if err != nil {
			slf.messageReciver.OnDisconnect(pclient.clientid, err)
			slf.WsServer.ReleaseClient(pclient)

			return
		}

		slf.messageReciver.OnRecvMsg(pclient.clientid, msgtype, message)
	}
}

func (slf *BaseMessageReciver) initReciver(messageReciver IMessageReceiver, websocketServer IWebsocketServer) {
	slf.messageReciver = messageReciver
	slf.WsServer = websocketServer
}

func (slf *BaseMessageReciver) OnConnected(clientid uint64) {
}

func (slf *BaseMessageReciver) OnDisconnect(clientid uint64, err error) {
}

func (slf *BaseMessageReciver) OnRecvMsg(clientid uint64, msgtype int, data []byte) {
}

func (slf *BaseMessageReciver) OnHandleHttp(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Upgrade(w, r, w.Header(), 1024, 1024)
	if err != nil {
		http.Error(w, "Could not open websocket connection", http.StatusBadRequest)
		return
	}

	pclient := slf.WsServer.CreateClient(conn)
	slf.messageReciver.OnConnected(pclient.clientid)
	go pclient.startSendMsg()
	go slf.startReadMsg(pclient)
}

func (slf *WebsocketServer) initRouterHandler() http.Handler {
	r := mux.NewRouter()

	for pattern, reciver := range slf.reciver {
		r.HandleFunc(pattern, reciver.messageReciver.OnHandleHttp)
	}

	cors := cors.AllowAll()
	return cors.Handler(r)
}

func (slf *WebsocketServer) SetWSS(certfile string, keyfile string) bool {
	slf.certfile = certfile
	slf.keyfile = keyfile
	slf.iswss = true
	return true
}
