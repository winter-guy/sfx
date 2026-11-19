// Command gamecast is a prototype of the supernode game-streaming core.
//
//	gamecast signal  -addr :8080
//	gamecast stream  -server ws://HOST:8080/ws -room r -in -        < video.h264
//	gamecast watch   -server ws://HOST:8080/ws -room r -host -out - | ffplay ...
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"

	"gamecast/internal/peer"
	"gamecast/internal/procstats"
	"gamecast/internal/proto"
	"gamecast/internal/signaling"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "signal":
		err = runSignal(ctx, os.Args[2:])
	case "stream":
		err = runPeer(ctx, proto.RoleStreamer, os.Args[2:])
	case "watch":
		err = runPeer(ctx, proto.RoleViewer, os.Args[2:])
	default:
		usage()
	}
	if err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

const (
	defaultStatsEvery = 0 // diagnostics only when asked for
	statsHelp         = "diagnostics: log this process's CPU, memory and goroutines this often, e.g. 5s (default off)"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  gamecast signal [-addr :8080]
  gamecast stream -server ws://HOST:8080/ws -room NAME [-in -|file.h264] [flags]
  gamecast watch  -server ws://HOST:8080/ws -room NAME [-host] [-out -|file.h264] [flags]

run "gamecast <command> -h" for flags`)
	os.Exit(2)
}

func runSignal(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("signal", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	stats := fs.Duration("stats", defaultStatsEvery, statsHelp)
	_ = fs.Parse(args)
	go procstats.Log(ctx, *stats, log.Printf)
	srv := &http.Server{Addr: *addr, Handler: signaling.NewServer().Handler()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		_ = srv.Close() // hijacked WebSocket conns aren't covered by Shutdown
	}()
	log.Printf("signaling server on %s (ws path /ws)", *addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func runPeer(ctx context.Context, role string, args []string) error {
	fs := flag.NewFlagSet(role, flag.ExitOnError)
	server := fs.String("server", "ws://localhost:8080/ws", "signaling server URL")
	room := fs.String("room", "default", "room name")
	name := fs.String("name", hostname(), "display name")
	upload := fs.Float64("upload", 0, "upload Mbps (0 = measure against the signaling server)")
	wired := fs.Bool("wired", false, "this PC is on wired Ethernet (preferred as supernode)")
	ice := fs.String("ice", "stun:stun.l.google.com:19302", "comma-separated STUN/TURN URLs (turn:user:pass@host:port)")
	loopback := fs.Bool("loopback", false, "allow 127.0.0.1 candidates (single-machine demo)")
	stats := fs.Duration("stats", defaultStatsEvery, statsHelp)

	// stream
	in := fs.String("in", "-", "stream: H.264 Annex-B input ('-' = stdin, or a file)")
	loop := fs.Bool("loop", false, "stream: replay -in file forever")
	fps := fs.Int("fps", 60, "stream: frame rate")
	bitrate := fs.Int("bitrate", signaling.DefaultBitrateKbps, "stream: encoder target kbps (used to size the supernode)")
	audioIn := fs.String("audio-in", "", "stream: receive game audio as Opus RTP on this UDP address, e.g. 127.0.0.1:5004 ('' = no audio)")
	audioKbps := fs.Int("audio-bitrate", 128, "stream: Opus bitrate in kbps (added to -bitrate when sizing the supernode)")

	// watch
	canHost := fs.Bool("host", false, "watch: volunteer this PC as supernode")
	out := fs.String("out", "-", "watch: write received H.264 here ('-' = stdout, '' = discard)")
	audioOut := fs.String("audio-out", "", "watch: write received game audio as Ogg/Opus here (file or named pipe; '-' = stdout if -out isn't; '' = none)")
	_ = fs.Parse(args)
	go procstats.Log(ctx, *stats, log.Printf)

	needUpload := role == proto.RoleStreamer || *canHost
	if *upload == 0 && needUpload {
		log.Printf("measuring upload (3s)…")
		mbps, err := peer.MeasureUploadMbps(ctx, *server, 3*time.Second)
		if err != nil {
			log.Printf("upload test failed (%v); reporting 0 Mbps", err)
		} else {
			*upload = mbps
			log.Printf("upload ≈ %.0f Mbps", mbps)
		}
	}

	cfg := peer.Config{
		ServerURL: *server, Room: *room, Name: *name, Role: role,
		UploadMbps: *upload, Wired: *wired, CanHost: *canHost,
		BitrateKbps:     *bitrate,
		ICEServers:      parseICE(*ice),
		IncludeLoopback: *loopback,
		FPS:             *fps,
	}

	if role == proto.RoleStreamer && *audioIn != "" {
		src, err := peer.ListenRTPAudio(*audioIn)
		if err != nil {
			return err
		}
		defer src.Close()
		go func() { <-ctx.Done(); src.Close() }()
		cfg.Audio = src
		cfg.BitrateKbps += *audioKbps
		log.Printf("game audio: listening for Opus RTP on %s", src.Addr())
	}
	if role == proto.RoleViewer && *audioOut != "" {
		switch *audioOut {
		case "-":
			if *out == "-" {
				return fmt.Errorf("-audio-out - and -out - can't both use stdout")
			}
			cfg.AudioOutput = os.Stdout
		default:
			f, err := os.Create(*audioOut)
			if err != nil {
				return err
			}
			defer f.Close()
			cfg.AudioOutput = f
		}
	}

	if role == proto.RoleStreamer {
		switch {
		case *in == "-":
			src, err := peer.NewAnnexBSource(os.Stdin)
			if err != nil {
				return err
			}
			cfg.Source = src // live pipe: the encoder sets the pace
		case *loop:
			cfg.Source, cfg.Pace = &peer.LoopingFileSource{Path: *in}, true
		default:
			f, err := os.Open(*in)
			if err != nil {
				return err
			}
			defer f.Close()
			src, err := peer.NewAnnexBSource(f)
			if err != nil {
				return err
			}
			cfg.Source, cfg.Pace = src, true
		}
	} else {
		switch *out {
		case "":
			cfg.Output = io.Discard
		case "-":
			cfg.Output = os.Stdout
		default:
			f, err := os.Create(*out)
			if err != nil {
				return err
			}
			defer f.Close()
			cfg.Output = f
		}
	}

	c, err := peer.New(cfg)
	if err != nil {
		return err
	}
	return c.Run(ctx)
}

func parseICE(s string) []webrtc.ICEServer {
	var out []webrtc.ICEServer
	for _, u := range strings.Split(s, ",") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		// turn:user:pass@host:port
		if strings.HasPrefix(u, "turn") {
			scheme, rest, _ := strings.Cut(u, ":")
			if creds, host, ok := strings.Cut(rest, "@"); ok {
				user, pass, _ := strings.Cut(creds, ":")
				out = append(out, webrtc.ICEServer{URLs: []string{scheme + ":" + host}, Username: user, Credential: pass})
				continue
			}
		}
		out = append(out, webrtc.ICEServer{URLs: []string{u}})
	}
	return out
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}
