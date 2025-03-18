// SPDX-License-Identifier: MIT

//go:build !js
// +build !js

// whip-whep demonstrates how to use the WHIP/WHEP specifications to exchange SPD descriptions
// and stream media to a WebRTC client in the browser or OBS.
package main

import (
	"bytes"
	"fmt"
	"github.com/google/uuid"
	ice "github.com/pion/ice/v4"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	webrtc "github.com/pion/webrtc/v4"
)

// nolint: gochecknoglobals
var (
	peerConnectionConfiguration = webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}
)

var settingEngine *webrtc.SettingEngine
var api *webrtc.API

func init() {
	// Create a SettingEngine, this allows non-standard WebRTC behavior
	settingEngine = &webrtc.SettingEngine{}
	udp := true

	if udp {
		// Configure our SettingEngine to use our UDPMux. By default a PeerConnection has
		// no global state. The API+SettingEngine allows the user to share state between them.
		// In this case we are sharing our listening port across many.
		// Listen on UDP Port 8443, will be used for all WebRTC traffic
		mux, err := ice.NewMultiUDPMuxFromPort(8443, ice.UDPMuxFromPortWithNetworks(ice.NetworkTypeUDP4))
		if err != nil {
			panic(err)
		}
		fmt.Printf("Listening for WebRTC traffic at %d\n", 8443)
		settingEngine.SetICEUDPMux(mux)
	} else {
		tcpListener, err := net.ListenTCP("tcp", &net.TCPAddr{
			IP:   net.IP{0, 0, 0, 0},
			Port: 8443,
		})
		if err != nil {
			panic(err)
		}

		fmt.Printf("Listening for ICE TCP at %s\n", tcpListener.Addr())

		tcpMux := webrtc.NewICETCPMux(nil, tcpListener, 8)
		settingEngine.SetICETCPMux(tcpMux)
		settingEngine.SetNetworkTypes([]webrtc.NetworkType{
			webrtc.NetworkTypeTCP4,
		})
	}
}

func init() {
	var err error
	err, api = prepareEngine()
	if err != nil {
		panic(err)
	}
}

func prepareEngine() (error, *webrtc.API) {
	mediaEngine := &webrtc.MediaEngine{}

	// Setup the codecs you want to use.
	// We'll only use H264 but you can also define your own
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1",
			RTCPFeedback: nil,
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	}

	// Create a InterceptorRegistry. This is the user configurable RTP/RTCP Pipeline.
	// This provides NACKs, RTCP Reports and other features. If you use `webrtc.NewPeerConnection`
	// this is enabled by default. If you are manually managing You MUST create a InterceptorRegistry
	// for each PeerConnection.
	interceptorRegistry := &interceptor.Registry{}

	// Register a intervalpli factory
	// This interceptor sends a PLI every 3 seconds. A PLI causes a video keyframe to be generated by the sender.
	// This makes our video seekable and more error resilent, but at a cost of lower picture quality and higher bitrates
	// A real world application should process incoming RTCP packets from viewers and forward them to senders
	intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
	if err != nil {
		panic(err)
	}
	interceptorRegistry.Add(intervalPliFactory)

	// Use the default set of Interceptors
	if err = webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		panic(err)
	}

	// Create the API object with the MediaEngine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithSettingEngine(*settingEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry))
	return err, api
}

var mapOfTracks = make(map[string]*webrtc.TrackLocalStaticRTP)

func MakeAndHoldVideoTrack(id string) *webrtc.TrackLocalStaticRTP {
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus,
	}, "audio", "pion")
	if err != nil {
		panic(err)
	}
	mapOfTracks[id] = track
	return track
}

type Query struct {
	Room string `uri:"room" binding:"required"`
	User string `uri:"user" binding:"required"`
}

type RoomId string

type User string

type Room struct {
	Caller User `json:"callerId"`
	Callee User `json:"calleeId"`
}

func (q Query) String() string {
	return fmt.Sprintf("%s-%s", q.Room, q.User)
}

type bodyLogWriter struct {
	gin.ResponseWriter
	body *bytes.Buffer
}

func (w bodyLogWriter) Write(b []byte) (int, error) {
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

var mutex sync.Mutex
var cache = make(map[RoomId]Room)

// nolint:gocognit
func main() {
	r := gin.Default()
	//log request and response body
	r.Use(func(c *gin.Context) {
		// Read the request body
		bodyBytes, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.AbortWithError(http.StatusInternalServerError, err)
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes)) // Restore the body for further use
		fmt.Println("Request Body: ", string(bodyBytes))

		// Capture the response body
		responseWriter := &bodyLogWriter{body: bytes.NewBufferString(""), ResponseWriter: c.Writer}
		c.Writer = responseWriter

		c.Next()

		fmt.Println("Response Body: ", responseWriter.body.String())
	})
	r.Static("/", ".")
	r.POST("/room/create", createRoomHandler)
	r.POST("/room/:room/init", initRoomHandler)
	r.POST("/room/:room", getRoomHandler)
	r.POST("/whep/:room/:user", whepHandler)
	r.POST("/whip/:room/:user", whipHandler)

	fmt.Println("Open http://localhost:8080 to access this demo")
	panic(r.Run("0.0.0.0:8080"))
}

func getRoomHandler(c *gin.Context) {
	mutex.Lock()
	defer mutex.Unlock()
	room := struct {
		Room string `uri:"room" binding:"required"`
	}{}
	if err := c.ShouldBindUri(&room); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	roomInfo, ok := cache[RoomId(room.Room)]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "room not found"})
	}
	c.JSON(http.StatusOK, roomInfo)
	return
}

func createRoomHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"room": uuid.New().String()})
	return
}

func initRoomHandler(c *gin.Context) {
	mutex.Lock()
	defer mutex.Unlock()
	room := struct {
		Room string `uri:"room" binding:"required"`
	}{}
	if err := c.ShouldBindUri(&room); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	caller, callee := User(uuid.New().String()), User(uuid.New().String())
	cache[RoomId(room.Room)] = Room{
		Caller: caller,
		Callee: callee,
	}
	c.JSON(http.StatusOK, gin.H{
		"callerId": caller,
		"calleeId": caller,
	})
	return
}

func whipHandler(c *gin.Context) {
	var query Query
	if err := c.ShouldBindUri(&query); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Read the offer from HTTP Request
	offer, err := io.ReadAll(c.Request.Body)
	if err != nil {
		panic(err)
	}

	// Create a MediaEngine object to configure the supported codec
	err, api = prepareEngine()
	if err != nil {
		panic(err)
	}

	// Prepare the configuration

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(peerConnectionConfiguration)
	if err != nil {
		panic(err)
	}

	// Allow us to receive 1 video trac
	if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	}

	// Set a handler for when a new remote track starts, this handler saves buffers to disk as
	// an ivf file, since we could have multiple video tracks we provide a counter.
	// In your application this is where you would handle/process video
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) { //nolint: revive
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				panic(err)
			}

			if _, ok := mapOfTracks[query.String()]; !ok {
				MakeAndHoldVideoTrack(query.String())
			}
			if err = mapOfTracks[query.String()].WriteRTP(pkt); err != nil {
				panic(err)
			}
		}
	})

	// Send answer via HTTP Response
	writeAnswer(c, peerConnection, offer, "/whip")
}

func whepHandler(c *gin.Context) {
	var query Query
	if err := c.ShouldBindUri(&query); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	offer, err := io.ReadAll(c.Request.Body)
	if err != nil {
		panic(err)
	}

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(peerConnectionConfiguration)
	if err != nil {
		panic(err)
	}

	// Add Video Track that is being written to from WHIP Session
	for i := 0; i < 10 && mapOfTracks[query.String()] == nil; i++ {
		time.Sleep(1 * time.Second)
	}
	if mapOfTracks[query.String()] == nil {
		c.Status(http.StatusNotFound)
		return
	}
	rtpSender, err := peerConnection.AddTrack(mapOfTracks[query.String()])
	if err != nil {
		panic(err)
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	// Send answer via HTTP Response
	writeAnswer(c, peerConnection, offer, "/whep")
}

func writeAnswer(c *gin.Context, peerConnection *webrtc.PeerConnection, offer []byte, path string) {
	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("ICE Connection State has changed: %s\n", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateFailed {
			_ = peerConnection.Close()
		}
	})

	if err := peerConnection.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: string(offer),
	}); err != nil {
		panic(err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	} else if err = peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	// WHIP+WHEP expects a Location header and a HTTP Status Code of 201
	c.Header("Location", path)
	c.Status(http.StatusCreated)

	// Write Answer with Candidates as HTTP Response
	c.String(http.StatusCreated, peerConnection.LocalDescription().SDP)
}
