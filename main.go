package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-chi/cors"
	"github.com/gordonklaus/portaudio"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/sirupsen/logrus"
	"gopkg.in/hraban/opus.v2"
)

func main() {
	log := logrus.New()
	log.SetLevel(logrus.DebugLevel)

	// cancellation
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGABRT)
	defer cancel()

	numClients := new(uint64)
	clientConnected := make(chan struct{})
	track := getTrack(ctx, log, numClients, clientConnected)

	mux := http.NewServeMux()
	mux.Handle("/sdp", handleClient(log, track, numClients, clientConnected))
	server := http.Server{
		Addr:    ":8080",
		Handler: cors.AllowAll().Handler(mux),
	}

	go func() {
		<-ctx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		defer cancel()
		server.Shutdown(ctx)
	}()

	log.Info("starting server on port 8080")
	err := server.ListenAndServe()
	if err != http.ErrServerClosed {
		panic(err)
	}
}

var config = webrtc.Configuration{
	ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
}

func handleClient(
	log logrus.FieldLogger,
	track *webrtc.TrackLocalStaticSample,
	numClients *uint64,
	clientConnected chan<- struct{},
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		log.Debug("got Request")

		dec := json.NewDecoder(r.Body)
		offer := webrtc.SessionDescription{}
		if err := dec.Decode(&offer); err != nil {
			http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
			return
		}
		log.Debug("decoded Session Description")

		conn, err := webrtc.NewPeerConnection(config)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Debug("created peer connection")

		conn.OnConnectionStateChange(func(pcs webrtc.PeerConnectionState) {
			switch pcs {
			case webrtc.PeerConnectionStateConnected:
				newCount := atomic.AddUint64(numClients, 1)
				if newCount == 1 {
					// signal client connected
					clientConnected <- struct{}{}
				}
			case webrtc.PeerConnectionStateDisconnected:
				atomic.AddUint64(numClients, ^uint64(0))
			}
		})

		rtpSender, err := conn.AddTrack(track)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Debug("added track")

		// Read incoming RTCP packets
		// Before these packets are returned they are processed by interceptors. For things
		// like NACK this needs to be called.
		go func() {
			rtcpBuf := make([]byte, 1500)
			for {
				if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
					log.Debug("rtcp done")
					return
				}
			}
		}()

		err = conn.SetRemoteDescription(offer)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Debug("remote description set")

		answer, err := conn.CreateAnswer(nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Debug("asnwer created")

		gatherComplete := webrtc.GatheringCompletePromise(conn)

		// Sets the LocalDescription, and starts our UDP listeners
		err = conn.SetLocalDescription(answer)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Debug("set local description")

		// Block until ICE Gathering is complete, disabling trickle ICE
		// we do this because we only can exchange one signaling message
		// in a production application you should exchange ICE Candidates via OnICECandidate
		<-gatherComplete

		if err := json.NewEncoder(w).Encode(&answer); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Debug("answer encoded and sent to client")
	}
}

const (
	sampleRate    = 48000
	frameDuration = time.Duration(float32(time.Millisecond) * float32(2.5))
	channelCount  = 1
)

var frameSize = uint64(frameDuration.Seconds() * sampleRate * channelCount)

func getTrack(
	ctx context.Context,
	log logrus.FieldLogger,
	numClients *uint64,
	clientConnected <-chan struct{},
) *webrtc.TrackLocalStaticSample {
	portaudio.Initialize()

	opusEnc, err := opus.NewEncoder(sampleRate, channelCount, opus.AppVoIP)
	if err != nil {
		panic(err)
	}

	// buffers
	inBuf := make([]int16, frameSize)
	encBuf := make([]byte, 1024)

	devices, err := portaudio.Devices()
	if err != nil {
		panic(err)
	}
	var selectedDev *portaudio.DeviceInfo
	if len(os.Args) > 1 {
		for _, dev := range devices {
			if dev.Name == os.Args[1] {
				if dev.MaxInputChannels < channelCount {
					log.WithField("channels", dev.MaxInputChannels).Fatal("Device not suitable for recording")
				}
				selectedDev = dev
			}
		}
		if selectedDev == nil {
			log.WithField("name", os.Args[0]).Fatal("dev not found")
		}
	} else {
		dev, err := portaudio.DefaultInputDevice()
		if err != nil {
			log.WithError(err).Fatal("Failed to find default input device")
		}
		selectedDev = dev
	}

	// open mic source
	params := portaudio.LowLatencyParameters(selectedDev, nil)
	params.Input.Channels = channelCount
	params.SampleRate = sampleRate
	params.FramesPerBuffer = len(inBuf)
	stream, err := portaudio.OpenStream(params, inBuf)
	if err != nil {
		panic(err)
	}

	// Create a audio track
	audioTrack, audioTrackErr := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if audioTrackErr != nil {
		panic(audioTrackErr)
	}

	go func() {
		defer portaudio.Terminate()
		defer stream.Close()

		started := false
		for {
			if ctx.Err() != nil {
				return
			}

			if atomic.LoadUint64(numClients) < 1 {
				log.Info("Waiting for clients to connect")

				if started {
					if err := stream.Abort(); err != nil {
						panic(err)
					}
					started = false
				}

				select {
				case <-ctx.Done():
					return
				case <-clientConnected:
				}

				log.Info("Client connected. Starting to stream")
			}

			// drain channel
			select {
			case <-clientConnected:
			default:
			}

			if !started {
				if err := stream.Start(); err != nil {
					panic(err)
				}
				started = true
			}

			if err := stream.Read(); err != nil {
				panic(err)
			}
			log.Debug("Read frame from input")

			// encode to opus
			size, err := opusEnc.Encode(inBuf, encBuf)
			if err != nil {
				panic(err)
			}
			log.WithField("size", size).Debug("Encoded to opus")

			if err := audioTrack.WriteSample(media.Sample{Data: encBuf[:size], Duration: frameDuration}); err != nil {
				log.WithError(err).Warn("one peer failed")
			}
		}
	}()

	return audioTrack
}
