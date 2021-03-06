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
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"gopkg.in/hraban/opus.v2"
)

var (
	currentClients = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "streama_current_clients",
		Help: "The current number of viewers",
	})
	totalClients = promauto.NewCounter(prometheus.CounterOpts{
		Name: "streama_total_clients",
		Help: "The total number of viewers",
	})
)

func main() {
	log := logrus.New()
	log.SetLevel(logrus.DebugLevel)

	// cancellation
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGABRT)
	defer cancel()

	log.Info("setting up audio...")
	addClient, err := setupAudio(ctx, log)
	if err != nil {
		log.WithError(err).Fatal("failed to setup audio recording")
	}

	mux := http.NewServeMux()
	mux.Handle("/sdp", handleClient(log, addClient))
	mux.Handle("/metrics", promhttp.Handler())
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
	err = server.ListenAndServe()
	if err != http.ErrServerClosed {
		panic(err)
	}
}

var config = webrtc.Configuration{
	ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
}

func handleClient(
	log logrus.FieldLogger,
	clientConnected chan<- *conn,
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

		rtcConn, err := webrtc.NewPeerConnection(config)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Debug("created peer connection")

		iceCandidates := make(chan *webrtc.ICECandidate)
		rtcConn.OnICECandidate(func(i *webrtc.ICECandidate) {
			if i == nil {
				close(iceCandidates)
			} else {
				iceCandidates <- i
			}
		})

		if err := initConn(log, rtcConn, clientConnected); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Debug("created internal connection tracking")

		err = rtcConn.SetRemoteDescription(offer)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Debug("remote description set")

		answer, err := rtcConn.CreateAnswer(nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Debug("answer created")

		// Sets the LocalDescription, and starts our UDP listeners
		err = rtcConn.SetLocalDescription(answer)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Debug("set local description")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "response writer is not flushable", http.StatusInternalServerError)
			return
		}

		enc := json.NewEncoder(w)
		if err := enc.Encode(&answer); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		flusher.Flush()
		log.Debug("answer encoded and sent to client")

		candidateCount := 0
		for {
			select {
			case <-r.Context().Done():
				return

			case candidate, open := <-iceCandidates:
				if !open {
					log.WithField("candidates", candidateCount).Debug("all candidates sent")
					return
				}

				candidateInit := candidate.ToJSON()
				err := enc.Encode(&candidateInit)
				if err != nil {
					w.Write([]byte(`{ "error": "failed to encode candidate" }`))
					return
				}
				flusher.Flush()
				candidateCount++
			}
		}
	}
}

const (
	sampleRate    = 48000
	frameDuration = time.Duration(10 * time.Millisecond)
	channelCount  = 2
)

var frameSize = uint64(frameDuration.Seconds() * sampleRate * channelCount)

type ConnectionState uint64

func (s *ConnectionState) Set(state ConnectionState) {
	atomic.StoreUint64((*uint64)(s), uint64(state))
}

func (s *ConnectionState) Get() ConnectionState {
	return ConnectionState(atomic.LoadUint64((*uint64)(s)))
}

const (
	ConnectionStateDisconnected ConnectionState = iota
	ConnectionStateConnected
	ConnectionStateClosed
)

type conn struct {
	state  *ConnectionState
	frames chan []byte
}

func initConn(log logrus.FieldLogger, rtcConn *webrtc.PeerConnection, clientConnected chan<- *conn) error {
	// Create a audio track
	audioTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: sampleRate,
			Channels:  channelCount,
		},
		"audio", "pion",
	)
	if err != nil {
		return fmt.Errorf("failed to create track: %w", err)
	}

	frames := make(chan []byte, 10)
	go func() {
		for {
			frame, ok := <-frames
			if !ok {
				return
			}
			err := audioTrack.WriteSample(media.Sample{Data: frame, Duration: frameDuration})
			if err != nil {
				log.WithError(err).Error("failed to write sample")
			}
		}
	}()

	state := new(ConnectionState)
	conn := &conn{state: state, frames: frames}
	rtcConn.OnConnectionStateChange(func(pcs webrtc.PeerConnectionState) {
		switch pcs {
		case webrtc.PeerConnectionStateConnected:
			state.Set(ConnectionStateConnected)
			clientConnected <- conn
			currentClients.Inc()
			totalClients.Inc()
		case webrtc.PeerConnectionStateDisconnected:
			state.Set(ConnectionStateDisconnected)
			currentClients.Dec()
		case webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateFailed:
			state.Set(ConnectionStateClosed)
			currentClients.Dec()
		}
	})

	rtpSender, err := rtcConn.AddTrack(audioTrack)
	if err != nil {
		return fmt.Errorf("failed to add track: %w", err)
	}

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

	return nil
}

type clientStat struct {
	sent    uint64
	dropped uint64
}

func setupAudio(
	ctx context.Context,
	log logrus.FieldLogger,
) (chan<- *conn, error) {
	portaudio.Initialize()

	opusEnc, err := opus.NewEncoder(sampleRate, channelCount, opus.AppVoIP)
	if err != nil {
		return nil, errors.Wrap(err, "failed setting up encoder")
	}

	// buffers
	inBuf := make([]int16, frameSize)
	encBuf := make([]byte, 1024)

	devices, err := portaudio.Devices()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get devices")
	}
	var selectedDev *portaudio.DeviceInfo
	for _, dev := range devices {
		log := log.WithFields(logrus.Fields{
			"device_name":         dev.Name,
			"hostapi_name":        dev.HostApi.Name,
			"input_channels":      dev.MaxInputChannels,
			"default_sample_rate": dev.DefaultSampleRate,
		})
		log.Debug("possible device found")
		if len(os.Args) > 1 {
			if dev.Name == os.Args[1] {
				if dev.MaxInputChannels < channelCount {
					log.Fatal("Device not suitable for recording")
				}
				log.Info("desired device found")
				selectedDev = dev
			}
		}
	}
	if selectedDev == nil {
		if len(os.Args) > 1 {
			log.WithField("name", os.Args[1]).Fatal("desired device not found")
		}

		dev, err := portaudio.DefaultInputDevice()
		if err != nil {
			log.WithError(err).Fatal("failed to use default input device")
		}
		log.WithFields(logrus.Fields{
			"device_name":         dev.Name,
			"hostapi_name":        dev.HostApi.Name,
			"input_channels":      dev.MaxInputChannels,
			"default_sample_rate": dev.DefaultSampleRate,
		}).Info("selected default audio device")
		selectedDev = dev
	}

	// open mic source
	params := portaudio.LowLatencyParameters(selectedDev, nil)
	params.Input.Channels = channelCount
	params.SampleRate = sampleRate
	params.FramesPerBuffer = len(inBuf)
	stream, err := portaudio.OpenStream(params, inBuf)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open stream")
	}

	clientConnected := make(chan *conn)
	go func() {
		defer portaudio.Terminate()
		defer stream.Close()

		started := false
		nextID := uint64(0)
		clients := make(map[uint64]*conn)
		stats := make(map[uint64]*clientStat)
		lastStatsOutput := time.Now()

		for {
			if ctx.Err() != nil {
				return
			}

			if len(clients) == 0 {
				if started {
					err := stream.Abort()
					if err != nil {
						panic(err)
					}
					started = false
				}

				log.Info("Waiting for clients to connect...")
				select {
				case <-ctx.Done():
					return
				case conn := <-clientConnected:
					clients[nextID] = conn
					stats[nextID] = &clientStat{}
					nextID++
				}
				log.Info("Client connected. Starting to stream")
			} else {
				// add new clients
				select {
				case conn := <-clientConnected:
					clients[nextID] = conn
					stats[nextID] = &clientStat{}
					nextID++
				default: // non blocking
				}
			}

			if !started {
				if err := stream.Start(); err != nil {
					panic(err)
				}
				started = true
			}

			if err := stream.Read(); err != nil {
				for id, stats := range stats {
					log.WithFields(logrus.Fields{
						"id":      id,
						"sent":    stats.sent,
						"dropped": stats.dropped,
					}).Info("statistics")
				}
				log.WithError(err).Fatal("failed to read audio input")
			}

			// encode to opus
			encSize, err := opusEnc.Encode(inBuf, encBuf)
			if err != nil {
				for id, stats := range stats {
					log.WithFields(logrus.Fields{
						"id":      id,
						"sent":    stats.sent,
						"dropped": stats.dropped,
					}).Info("statistics")
				}
				log.WithError(err).Fatal("failed to encode audio")
			}

			printStats := time.Since(lastStatsOutput) > time.Second*5

			for id, conn := range clients {
				switch conn.state.Get() {
				case ConnectionStateClosed:
					log.WithFields(logrus.Fields{
						"id":      id,
						"sent":    stats[id].sent,
						"dropped": stats[id].dropped,
					}).Info("client connection closed")
					close(conn.frames)
					delete(stats, id)
					delete(clients, id)
					continue
				case ConnectionStateDisconnected:
					continue
				}

				select {
				case conn.frames <- encBuf[:encSize]:
					stats[id].sent++
				default:
					stats[id].dropped++
				}

				if printStats {
					log.WithFields(logrus.Fields{
						"id":      id,
						"sent":    stats[id].sent,
						"dropped": stats[id].dropped,
					}).Info("statistics")
				}
			}

			if printStats {
				lastStatsOutput = time.Now()
			}

		}

	}()

	return clientConnected, nil
}
