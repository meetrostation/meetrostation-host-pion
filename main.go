package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"encoding/base64"
	"encoding/json"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/warthog618/go-gpiocdev"
)

type Driver struct {
	forward  bool
	backward bool
	left     bool
	right    bool
	pins     *gpiocdev.Lines
}

func (driver *Driver) initialize() error {

	var err error
	driver.pins, err = gpiocdev.RequestLines("gpiochip0", []int{5, 26, 17, 27}, gpiocdev.AsOutput(0, 0, 0, 0))

	if err != nil {
		return err
	}

	return nil
}

func (driver *Driver) cleanup() error {
	driver.pins.SetValues([]int{0, 0, 0, 0})

	var err error
	err = driver.pins.Close()

	if err != nil {
		return err
	}

	return nil
}

func (driver *Driver) drive() {
	orientation := 0 // negative - left, positive - right
	if driver.left && driver.right {
		orientation = 0
	} else if !driver.left && !driver.right {
		orientation = 0
	} else if driver.left {
		orientation = -1
	} else {
		orientation = 1
	}

	direction := 0 // negative - back, positive - forward
	if driver.forward && driver.backward {
		direction = 0
	} else if !driver.forward && !driver.backward {
		direction = 0
	} else if driver.forward {
		direction = 1
	} else {
		direction = -1
	}

	controlLeft := 0
	controlRight := 0

	//  2,0     1,1     0,2
	//  1,-1    0,0     -1,1
	//  0,-2    -1,-1   -2,0

	if orientation > 0 {
		controlLeft = 1
		controlRight = -1
	} else if orientation == 0 {
		controlLeft = 0
		controlRight = 0
	} else {
		controlLeft = -1
		controlRight = 1
	}

	if direction > 0 {
		controlLeft += 1
		controlRight += 1
	} else if direction == 0 {
	} else {
		controlLeft -= 1
		controlRight -= 1
	}

	// controlLeft *= 5
	// controlRight *= 5

	pins := []int{0, 0, 0, 0}
	if controlLeft > 0 {
		pins[0] = 1
		pins[1] = 0
	} else if controlLeft < 0 {
		pins[0] = 0
		pins[1] = 1
	} else {
		pins[0] = 0
		pins[1] = 0
	}

	if controlRight > 0 {
		pins[2] = 1
		pins[3] = 0
	} else if controlRight < 0 {
		pins[2] = 0
		pins[3] = 1
	} else {
		pins[2] = 0
		pins[3] = 0
	}

	driver.pins.SetValues(pins)
}

type Peer struct {
	peerConnection        *webrtc.PeerConnection
	localVideoTrack       *webrtc.TrackLocalStaticRTP
	localAudioTrack       *webrtc.TrackLocalStaticRTP
	dataChannel           *webrtc.DataChannel
	remoteVideoConnection *net.UDPConn
	remoteAudioConnection *net.UDPConn
	gatherComplete        <-chan struct{}
}

func (peer *Peer) Close(index int) {
	if peer.peerConnection != nil {
		err := peer.peerConnection.Close()
		peer.peerConnection = nil

		if err != nil {
			fmt.Fprintf(os.Stderr,
				"conn %d: peerConnection.Close - %s\n",
				index,
				err)
		}
	}

	if peer.localVideoTrack != nil {
		peer.localVideoTrack = nil
	}

	if peer.localAudioTrack != nil {
		peer.localAudioTrack = nil
	}

	if peer.dataChannel != nil {
		peer.dataChannel.OnMessage(func(message webrtc.DataChannelMessage) {
		})

		err := peer.dataChannel.Close()
		peer.dataChannel = nil

		if err != nil {
			fmt.Fprintf(os.Stderr,
				"conn %d: dataChannel.Close - %s\n",
				index,
				err)
		}
	}

	if peer.remoteAudioConnection != nil {
		err := peer.remoteAudioConnection.Close()
		peer.remoteAudioConnection = nil

		if err != nil {
			fmt.Fprintf(os.Stderr,
				"conn %d: remoteAudioConnection.Close - %s\n",
				index,
				err)
		}
	}

	if peer.remoteVideoConnection != nil {
		err := peer.remoteVideoConnection.Close()
		peer.remoteVideoConnection = nil

		if err != nil {
			fmt.Fprintf(os.Stderr,
				"conn %d: remoteVideoConnection.Close - %s\n",
				index,
				err)
		}
	}
}

func (peer *Peer) CloseRemoteConnections(index int) {
	if peer.dataChannel != nil {
		peer.dataChannel.OnMessage(func(message webrtc.DataChannelMessage) {
		})

		err := peer.dataChannel.Close()
		peer.dataChannel = nil

		if err != nil {
			fmt.Fprintf(os.Stderr,
				"conn %d: dataChannel.Close - %s\n",
				index,
				err)
		}
	}

	if peer.remoteAudioConnection != nil {
		err := peer.remoteAudioConnection.Close()
		peer.remoteAudioConnection = nil

		if err != nil {
			fmt.Fprintf(os.Stderr,
				"conn %d: remoteAudioConnection.Close - %s\n",
				index,
				err)
		}
	}

	if peer.remoteVideoConnection != nil {
		err := peer.remoteVideoConnection.Close()
		peer.remoteVideoConnection = nil

		if err != nil {
			fmt.Fprintf(os.Stderr,
				"conn %d: remoteVideoConnection.Close - %s\n",
				index,
				err)
		}
	}
}

func (peer *Peer) IsNull() bool {
	return (peer.peerConnection == nil ||
		peer.remoteAudioConnection == nil ||
		peer.remoteVideoConnection == nil ||
		peer.dataChannel == nil ||
		peer.localAudioTrack == nil ||
		peer.localVideoTrack == nil)
}

func startPeerConnection() (
	*webrtc.PeerConnection,
	*webrtc.TrackLocalStaticRTP,
	*webrtc.TrackLocalStaticRTP,
	*webrtc.DataChannel,
	error) {

	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264,
		},
		"video",
		"pion")

	if err != nil {
		peerConnection.Close()
		return nil, nil, nil, nil, err
	}

	rtpSender, err := peerConnection.AddTrack(videoTrack)
	if err != nil {
		peerConnection.Close()
		return nil, nil, nil, nil, err
	}
	_ = rtpSender

	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus,
		},
		"audio",
		"pion")

	if err != nil {
		peerConnection.Close()
		return nil, nil, nil, nil, err
	}

	rtpSender, err = peerConnection.AddTrack(audioTrack)
	if err != nil {
		peerConnection.Close()
		return nil, nil, nil, nil, err
	}
	_ = rtpSender

	// // Read incoming RTCP packets
	// // Before these packets are returned they are processed by interceptors. For things
	// // like NACK this needs to be called.
	// go func() {
	// 	rtcpBuf := make([]byte, 1500)
	// 	for {
	// 		if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
	// 			return
	// 		}
	// 	}
	// }()

	dataChannelOrdered := true
	dataChannelNegotiated := true
	var dataChannelID uint16 = 0
	dataChannelInit := webrtc.DataChannelInit{
		Ordered:    &dataChannelOrdered,
		Negotiated: &dataChannelNegotiated,
		ID:         &dataChannelID,
	}
	dataChannel, err := peerConnection.CreateDataChannel(
		"meetrostation", &dataChannelInit)
	if err != nil {
		peerConnection.Close()
		return nil, nil, nil, nil, err
	}

	// dataChannel.OnOpen(func() {
	// 	fmt.Fprintf(os.Stderr,
	// 		"data channel opened\n")
	// })
	// dataChannel.OnClose(func() {
	// 	fmt.Fprintf(os.Stderr,
	// 		"data channel closed\n")
	// })

	return peerConnection,
		videoTrack,
		audioTrack,
		dataChannel,
		nil
}

func newPeerConnection(peers *[]Peer) int {
	for {
		peerConnection,
			localVideoTrack,
			localAudioTrack,
			dataChannel,
			err := startPeerConnection()

		if err != nil {
			fmt.Fprintf(os.Stderr,
				"while setting up peer connection. will retry: %s\n",
				err)
			continue
		}

		*peers = append(*peers, Peer{
			peerConnection:        peerConnection,
			localVideoTrack:       localVideoTrack,
			localAudioTrack:       localAudioTrack,
			dataChannel:           dataChannel,
			remoteVideoConnection: nil,
			remoteAudioConnection: nil,
		})

		peerConnection.OnICEConnectionStateChange(
			func(connectionState webrtc.ICEConnectionState) {
				peerIndex := 0
				for {
					if peerIndex == len(*peers) {
						fmt.Fprintf(os.Stderr,
							"conn ?: state - %s\n",
							connectionState.String())
						return
					}
					if (*peers)[peerIndex].peerConnection == peerConnection {
						break
					}
					peerIndex++
				}

				fmt.Fprintf(os.Stderr,
					"conn %d: state - %s\n",
					peerIndex,
					connectionState.String())

				if connectionState == webrtc.ICEConnectionStateFailed ||
					connectionState == webrtc.ICEConnectionStateDisconnected ||
					connectionState == webrtc.ICEConnectionStateClosed {
					(*peers)[peerIndex].Close(peerIndex)
				}
			})

		return len(*peers) - 1
	}
}

func setupTracksAndDataHandlers(peers *[]Peer, peerIndex int, driver *Driver) {
	for index, peer := range *peers {
		if index == peerIndex {
			continue
		}

		peer.CloseRemoteConnections(index)
	}

	var localAddress *net.UDPAddr
	var err error

	localAddress, err = net.ResolveUDPAddr("udp", "127.0.0.1:")
	if err != nil {
		panic(fmt.Sprintf("logic: net.ResolveUDPAddr for local - %s", err))
	}

	var remoteAddressAudio *net.UDPAddr
	remoteAddressAudio, err = net.ResolveUDPAddr("udp", "127.0.0.1:4002")
	if err != nil {
		panic(fmt.Sprintf("logic: net.ResolveUDPAddr for remote audio - %s", err))
	}

	(*peers)[peerIndex].remoteAudioConnection, err = net.DialUDP("udp", localAddress, remoteAddressAudio)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"conn %d: audio - net.DialUDP - %s\n",
			peerIndex,
			err)
	}

	var remoteAddressVideo *net.UDPAddr
	remoteAddressVideo, err = net.ResolveUDPAddr("udp", "127.0.0.1:4004")
	if err != nil {
		panic(fmt.Sprintf("logic: net.ResolveUDPAddr for remote video - %s", err))
	}

	(*peers)[peerIndex].remoteVideoConnection, err = net.DialUDP("udp", localAddress, remoteAddressVideo)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"conn %d: video - net.DialUDP - %s\n",
			peerIndex,
			err)
	}

	(*peers)[peerIndex].peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		connection, payloadType := func(track *webrtc.TrackRemote) (*net.UDPConn, uint8) {
			if track.Kind().String() == "video" {
				return (*peers)[peerIndex].remoteVideoConnection, 96
			} else {
				return (*peers)[peerIndex].remoteAudioConnection, 111
			}
		}(track)

		buf := make([]byte, 1500)
		rtpPacket := &rtp.Packet{}
		for {
			if (*peers)[peerIndex].IsNull() {
				break
			}

			n, _, err := track.Read(buf)
			if err != nil {
				fmt.Fprintf(os.Stderr,
					"conn %d: track read - %s\n",
					peerIndex,
					err)
				break
			}

			err = rtpPacket.Unmarshal(buf[:n])
			if err != nil {
				fmt.Fprintf(os.Stderr,
					"conn %d: rtp packet unmarshal - %s\n",
					peerIndex,
					err)
			}
			rtpPacket.PayloadType = payloadType

			n, err = rtpPacket.MarshalTo(buf)
			if err != nil {
				fmt.Fprintf(os.Stderr,
					"conn %d: rtp packet marshal - %s\n",
					peerIndex,
					err)
			}

			_, err = connection.Write(buf[:n])
			if err != nil {
				var opError *net.OpError
				if errors.As(err, &opError) &&
					opError.Err.Error() == "write: connection refused" {
					continue
				}

				fmt.Fprintf(os.Stderr,
					"conn %d: rtp packet write - %s\n",
					peerIndex,
					err)

				break
			}
		}
	})

	(*peers)[peerIndex].dataChannel.OnClose(func() {
		driver.forward = false
		driver.backward = false
		driver.left = false
		driver.right = false
		driver.drive()
	})

	(*peers)[peerIndex].dataChannel.OnMessage(func(message webrtc.DataChannelMessage) {
		strData := string(message.Data)

		if strData == "rp" {
			driver.right = true
		} else if strData == "rr" {
			driver.right = false
		} else if strData == "lp" {
			driver.left = true
		} else if strData == "lr" {
			driver.left = false
		} else if strData == "up" {
			driver.forward = true
		} else if strData == "ur" {
			driver.forward = false
		} else if strData == "dp" {
			driver.backward = true
		} else if strData == "dr" {
			driver.backward = false
		}

		driver.drive()

		fmt.Fprintf(os.Stderr,
			"conn %d: data - %s\n",
			peerIndex,
			string(message.Data))
	})
}

func signalHostSetup(signalServer string,
	hostId string,
	peers []Peer,
	peerIndex int) {

	client := &http.Client{}

	for {
		request, err := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/api/host",
				signalServer),
			bytes.NewBufferString(
				fmt.Sprintf("{\"id\": \"%s\", \"description\": \"%s\"}",
					hostId,
					encode(
						peers[peerIndex].peerConnection.LocalDescription(),
					),
				),
			))
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"conn %d: while setting up hostId with signalling server: %s\n",
				peerIndex,
				err)
			continue
		}

		request.Header.Add("Content-type", "application/json; charset=UTF-8")

		hostSignal, err := client.Do(request)

		if err != nil {
			fmt.Fprintf(os.Stderr,
				"conn %d: while setting up hostId with signalling server: %s\n",
				peerIndex,
				err)
			continue
		}
		allOK := hostSignal.StatusCode == http.StatusOK
		hostSignal.Body.Close()

		if allOK {
			// var hostSignalBody map[string]interface{}

			// json.NewDecoder(hostSignal.Body).Decode(hostSignalBody)
			break
		} else {
			fmt.Fprintf(os.Stderr,
				"conn %d: while setting up hostId with signalling server: %s\n",
				peerIndex,
				"response status")
			continue
		}
	}
}

func signalWaitForGuest(signalServer string,
	hostId string,
	peerIndex int) webrtc.SessionDescription {

	client := &http.Client{}

	for {
		time.Sleep(1 * time.Second)

		params := url.Values{}
		params.Add("hostId", hostId)

		request, err := http.NewRequest(http.MethodGet,
			fmt.Sprintf(
				"%s/api/guest?%s",
				signalServer,
				params.Encode(),
			),
			nil)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"conn %d: while getting guest information with signalling server: %s\n",
				peerIndex,
				err)
			continue
		}

		guestSignal, err := client.Do(request)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"conn %d: while getting guest information with signalling server: %s\n",
				peerIndex,
				err)
			continue
		}

		var guestDescriptionObject map[string]string
		allOK := guestSignal.StatusCode == http.StatusOK
		if allOK {
			json.NewDecoder(guestSignal.Body).Decode(&guestDescriptionObject)
		}
		guestSignal.Body.Close()

		if allOK {
			guestDescription := guestDescriptionObject["guestDescription"]
			if guestDescription != "" {
				guestOffer := webrtc.SessionDescription{}
				decode(guestDescription, &guestOffer)

				return guestOffer
			}
		} else {
			fmt.Fprintf(os.Stderr,
				"conn %d: while getting guest information with signalling server: %s\n",
				peerIndex,
				"response status")
			continue
		}
	}
}

func streamLocalTrack(peers *[]Peer, video bool, port int) {
	listener, err := net.ListenUDP(
		"udp",
		&net.UDPAddr{
			IP:   net.ParseIP("127.0.0.1"),
			Port: port})

	if err != nil {
		fmt.Fprintf(os.Stderr,
			"net.ListenUDP, %s\n",
			err)
	}

	defer func() {
		if err = listener.Close(); err != nil {
			fmt.Fprintf(os.Stderr,
				"listener.Close, %s\n",
				err)
		}
	}()

	// Increase the UDP receive buffer size
	// Default UDP buffer sizes vary on different operating systems
	bufferSize := 300000 // 300KB
	err = listener.SetReadBuffer(bufferSize)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"listener.SetReadBuffer, %s\n",
			err)
	}

	inboundRTPPacket := make([]byte, 1600) // UDP MTU
	for {
		readBytes, _, err := listener.ReadFrom(inboundRTPPacket)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"listener.ReadFrom: %s\n",
				err)
		}

		// fmt.Println(readBytes)
		for peerIndex, peer := range *peers {
			track := func() *webrtc.TrackLocalStaticRTP {
				if video {
					return peer.localVideoTrack
				} else {
					return peer.localAudioTrack
				}
			}()

			if track == nil {
				continue
			}
			_, err = track.Write(inboundRTPPacket[:readBytes])
			if err != nil {
				if errors.Is(err, io.ErrClosedPipe) {
					peer.Close(peerIndex)
				}

				fmt.Fprintf(os.Stderr,
					"conn %d: while write to track: %s\n",
					peerIndex,
					err)
			}
		}
		// fmt.Println(writtenBytes)
	}
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr,
			"example usage: ./host-golang https://meetrostation.com \"secret host room id\"\n")
		return
	}
	signalServer := os.Args[1]
	hostId := os.Args[2]
	// signalServer := "https://meetrostation.com"
	// hostId := "secret host room id"

	var peers []Peer

	var driver Driver
	err := driver.initialize()
	if err != nil {
		fmt.Fprintf(os.Stderr, "while driver initialize: %s\n", err)
		return
	}

	interruptChannel := make(chan os.Signal)
	signal.Notify(interruptChannel, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-interruptChannel
		err := driver.cleanup()
		if err != nil {
			fmt.Fprintf(os.Stderr, "while driver cleanup: %s\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "successful driver cleanup.\n")
		}
		os.Exit(0)
	}()

	go streamLocalTrack(&peers, false, 3998)
	go streamLocalTrack(&peers, true, 4000)

	for {
		var err error
		peerIndex := newPeerConnection(&peers)

		var offerSessionDescription webrtc.SessionDescription

		for {
			offerSessionDescription, err = peers[peerIndex].peerConnection.CreateOffer(nil)

			if err != nil {
				fmt.Fprintf(os.Stderr, "while creating offer: %s\n", err)
				continue
			}
			break
		}

		// later will be locking untill this channel completes
		peers[peerIndex].gatherComplete = webrtc.GatheringCompletePromise(peers[peerIndex].peerConnection)

		for {
			err = peers[peerIndex].peerConnection.SetLocalDescription(offerSessionDescription)
			if err != nil {
				fmt.Fprintf(os.Stderr,
					"while setting local description: %s\n",
					err)
				continue
			}
			break
		}

		fmt.Fprintf(os.Stderr, "conn %d: waiting for all ice candidates\n", peerIndex)
		<-peers[peerIndex].gatherComplete

		fmt.Fprintf(os.Stderr, "conn %d: all ice candidates are received from stun server\n", peerIndex)

		signalHostSetup(signalServer,
			hostId,
			peers,
			peerIndex)

		fmt.Fprintf(os.Stderr, "conn %d: waiting for the peer to join\n", peerIndex)

		guestOffer := signalWaitForGuest(signalServer,
			hostId,
			peerIndex)

		setupTracksAndDataHandlers(&peers, peerIndex, &driver)

		peers[peerIndex].peerConnection.SetRemoteDescription(guestOffer)

		fmt.Fprintf(os.Stderr, "conn %d: joined: waiting for the ice connection\n", peerIndex)
		time.Sleep(1 * time.Second)
	}
}

// JSON encode + base64 a SessionDescription.
func encode(obj *webrtc.SessionDescription) string {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}

	return base64.StdEncoding.EncodeToString(b)
}

// Decode a base64 and unmarshal JSON into a SessionDescription.
func decode(in string, obj *webrtc.SessionDescription) {
	b, err := base64.StdEncoding.DecodeString(in)
	if err != nil {
		panic(err)
	}

	if err = json.Unmarshal(b, obj); err != nil {
		panic(err)
	}
}
