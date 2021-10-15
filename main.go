package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-chi/cors"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/oggreader"
	"github.com/sirupsen/logrus"
)

const oggPageDuration = time.Millisecond * 20

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

func getTrack(
	ctx context.Context,
	log logrus.FieldLogger,
	numClients *uint64,
	clientConnected <-chan struct{},
) *webrtc.TrackLocalStaticSample {
	// Create a audio track
	audioTrack, audioTrackErr := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if audioTrackErr != nil {
		panic(audioTrackErr)
	}

	go func() {
		if ctx.Err() != nil {
			return
		}

		if atomic.LoadUint64(numClients) < 1 {
			log.Info("Waiting for clients to connect")
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

		// Open a IVF file and start reading using our IVFReader
		file, oggErr := os.Open("test.ogg")
		if oggErr != nil {
			panic(oggErr)
		}

		// Open on oggfile in non-checksum mode.
		ogg, _, oggErr := oggreader.NewWith(file)
		if oggErr != nil {
			panic(oggErr)
		}

		// Wait for connection established
		//<-iceConnectedCtx.Done()

		// Keep track of last granule, the difference is the amount of samples in the buffer
		var lastGranule uint64

		// It is important to use a time.Ticker instead of time.Sleep because
		// * avoids accumulating skew, just calling time.Sleep didn't compensate for the time spent parsing the data
		// * works around latency issues with Sleep (see https://github.com/golang/go/issues/44343)
		ticker := time.NewTicker(oggPageDuration)
		for ; true; <-ticker.C {
			pageData, pageHeader, oggErr := ogg.ParseNextPage()
			if oggErr == io.EOF {
				fmt.Printf("All audio pages parsed and sent")
				os.Exit(0)
			}

			if oggErr != nil {
				panic(oggErr)
			}

			// The amount of samples is the difference between the last and current timestamp
			sampleCount := float64(pageHeader.GranulePosition - lastGranule)
			lastGranule = pageHeader.GranulePosition
			sampleDuration := time.Duration((sampleCount/48000)*1000) * time.Millisecond

			if err := audioTrack.WriteSample(media.Sample{Data: pageData, Duration: sampleDuration}); err != nil {
				log.WithError(err).Warn("one peer failed")
			}
		}
	}()

	return audioTrack
}
