//! voice: the voice chat audio engine, run as a sidecar by cast.
//!
//! cast (Go) carries voice over WebRTC; this process does the audio:
//! microphone → echo cancellation / noise suppression / AGC (sonora) → Opus →
//! RTP to cast, and RTP from cast (voices, game audio) → jitter
//! buffers → mix → speakers.
//!
//! Protocol with the parent process:
//! * stdout, once: `READY voice=127.0.0.1:P game=127.0.0.1:Q` — where to send
//!   voice RTP and game-audio RTP.
//! * stdin, line commands: `mute`, `unmute`, `quit`. EOF (parent gone) quits.
//! * mic audio goes as Opus RTP to the `--send` address.
//! * logs go to stderr.

mod engine;
mod io;
mod jitter;
mod opus;
mod resample;
mod rtp;

use std::io::{BufRead, ErrorKind, Write};
use std::net::UdpSocket;
use std::sync::mpsc;
use std::time::{Duration, Instant};

use anyhow::{Context, Result, bail};

use engine::{Engine, EngineConfig, FRAME, GameMode};

const USAGE: &str = "usage: voice --send HOST:PORT [options]
       voice --list-devices

  --send ADDR        where to send mic audio (Opus RTP), e.g. 127.0.0.1:6000
  --input SPEC       default | sine:FREQ | silence          (default: default)
  --output SPEC      default | raw:PATH | null              (default: default)
  --game MODE        play | reference | off                 (default: play)
                     reference: echo-cancel game audio that the OS already plays
  --bitrate BPS      Opus bitrate for the mic               (default: 32000)
  --muted            start muted
  --mic-delay MS     hold the mic back before echo cancellation (default: 80 with
                     --game reference, else 0); see engine::default_mic_delay_ms
  --no-aec --no-ns --no-agc   disable echo cancellation / noise suppression / AGC
  --echo-sim G:MS    test aid: leak G x speaker output into the mic MS ms later";

enum Cmd {
    Mute(bool),
    Quit,
}

fn main() {
    if let Err(e) = run() {
        eprintln!("voice: error: {e:#}");
        std::process::exit(1);
    }
}

fn run() -> Result<()> {
    let mut args = std::env::args().skip(1);
    let (mut send, mut input, mut output) = (None, "default".to_owned(), "default".to_owned());
    let mut cfg = EngineConfig {
        aec: true,
        noise_suppression: true,
        agc: true,
        game_mode: GameMode::Play,
        bitrate: 32_000,
        ssrc: engine::rand_u32(),
        echo_sim: None,
        mic_delay_ms: 0,
    };
    let mut muted = false;
    let mut mic_delay = None;
    while let Some(a) = args.next() {
        let mut val = || args.next().with_context(|| format!("{a} needs a value"));
        match a.as_str() {
            "--list-devices" => return io::list_devices(),
            "--send" => send = Some(val()?),
            "--input" => input = val()?,
            "--output" => output = val()?,
            "--game" => {
                cfg.game_mode = match val()?.as_str() {
                    "play" => GameMode::Play,
                    "reference" => GameMode::Reference,
                    "off" => GameMode::Off,
                    m => bail!("unknown --game mode {m:?}"),
                }
            }
            "--bitrate" => cfg.bitrate = val()?.parse().context("--bitrate")?,
            "--muted" => muted = true,
            "--mic-delay" => mic_delay = Some(val()?.parse().context("--mic-delay")?),
            "--no-aec" => cfg.aec = false,
            "--no-ns" => cfg.noise_suppression = false,
            "--no-agc" => cfg.agc = false,
            "--echo-sim" => {
                let v = val()?;
                let (g, ms) = v.split_once(':').context("--echo-sim GAIN:MS")?;
                cfg.echo_sim = Some((g.parse()?, ms.parse()?));
            }
            "-h" | "--help" => {
                println!("{USAGE}");
                return Ok(());
            }
            _ => bail!("unknown argument {a:?}\n{USAGE}"),
        }
    }
    let send = send.context(format!("--send is required\n{USAGE}"))?;
    cfg.mic_delay_ms = mic_delay.unwrap_or_else(|| engine::default_mic_delay_ms(cfg.game_mode));

    let voice_sock = UdpSocket::bind("127.0.0.1:0")?;
    let game_sock = UdpSocket::bind("127.0.0.1:0")?;
    voice_sock.set_nonblocking(true)?;
    game_sock.set_nonblocking(true)?;
    let out_sock = UdpSocket::bind("127.0.0.1:0")?;
    out_sock
        .connect(&send)
        .with_context(|| format!("--send {send}"))?;

    let mut input = io::Input::open(&input)?;
    let mut output = io::Output::open(&output)?;
    let mut engine = Engine::new(cfg.clone())?;
    engine.set_muted(muted);
    eprintln!(
        "voice: ready (aec={} ns={} agc={} game={:?} mic_delay={}ms muted={muted})",
        cfg.aec, cfg.noise_suppression, cfg.agc, cfg.game_mode, cfg.mic_delay_ms
    );

    // Handshake: tell the parent where to send packets.
    {
        let mut out = std::io::stdout().lock();
        writeln!(
            out,
            "READY voice={} game={}",
            voice_sock.local_addr()?,
            game_sock.local_addr()?
        )?;
        out.flush()?;
    }

    let (tx, rx) = mpsc::channel();
    std::thread::spawn(move || {
        for line in std::io::stdin().lock().lines() {
            let cmd = match line.as_deref().map(str::trim) {
                Ok("mute") => Cmd::Mute(true),
                Ok("unmute") => Cmd::Mute(false),
                Ok("quit") | Err(_) => break,
                Ok(_) => continue,
            };
            if tx.send(cmd).is_err() {
                return;
            }
        }
        let _ = tx.send(Cmd::Quit); // EOF: the parent went away
    });

    let mut buf = [0u8; 1500];
    let mut mic = vec![0.0f32; FRAME];
    let mut out = vec![0.0f32; FRAME * 2];
    let mut next = Instant::now();
    'run: loop {
        while let Ok(cmd) = rx.try_recv() {
            match cmd {
                Cmd::Mute(m) => {
                    engine.set_muted(m);
                    eprintln!("voice: {}", if m { "muted" } else { "unmuted" });
                }
                Cmd::Quit => break 'run,
            }
        }
        drain(&voice_sock, &mut buf, |p| engine.on_voice_packet(p));
        drain(&game_sock, &mut buf, |p| engine.on_game_packet(p));

        // Pace by the microphone's clock, or by a 10 ms timer without one.
        if input.is_device() {
            if !input.read(&mut mic) {
                std::thread::sleep(Duration::from_millis(1));
                continue;
            }
        } else {
            next += Duration::from_millis(10);
            let now = Instant::now();
            if next > now {
                std::thread::sleep(next - now);
            } else if now - next > Duration::from_millis(100) {
                next = now; // fell behind; don't burst
            }
            input.read(&mut mic);
        }

        // Echo path: what's queued for the speakers + what's queued from the
        // mic + typical device latency. AEC3 refines the estimate itself.
        let delay = if input.is_device() || output.buffered_ms() > 0 {
            output.buffered_ms() + input.buffered_ms() + 20
        } else {
            0
        };
        engine.tick(&mic, &mut out, delay, |p| {
            let _ = out_sock.send(p); // parent not listening yet: drop
        });
        output.write(&out);
    }
    output.flush();
    let s = engine.stats;
    eprintln!(
        "voice: stats voice_rx={} game_rx={} sent={} speakers={}",
        s.voice_packets, s.game_packets, s.sent_packets, s.speakers
    );
    Ok(())
}

fn drain(sock: &UdpSocket, buf: &mut [u8], mut f: impl FnMut(&[u8])) {
    loop {
        match sock.recv(buf) {
            Ok(n) => f(&buf[..n]),
            Err(e) if e.kind() == ErrorKind::WouldBlock => return,
            Err(e) if e.kind() == ErrorKind::ConnectionReset => continue, // Windows: ICMP from a previous send
            Err(e) => {
                eprintln!("voice: receive error: {e}");
                return;
            }
        }
    }
}
