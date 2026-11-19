//! The audio engine: one call per 10 ms does playback mixing, feeds the echo
//! canceller the far-end signal, cleans the microphone and encodes it.
//!
//! ```text
//!  voices (Opus RTP, per SSRC) ─▶ jitter ─▶┐
//!  game audio (Opus RTP) ────────▶ jitter ─▶┼▶ mix ─▶ speakers
//!                                          └──────▶ AEC render reference
//!  microphone ─▶ AEC + noise suppression + AGC ─▶ Opus ─▶ RTP ─▶ cast
//! ```

use std::collections::{HashMap, VecDeque};
use std::time::Duration;

use anyhow::Result;
use sonora::config::{EchoCanceller, GainController2, NoiseSuppression};
use sonora::{AudioProcessing, Config, StreamConfig};

use crate::jitter::{JitterBuffer, START_PACKETS};
use crate::opus::{Decoder, Encoder, SAMPLE_RATE};
use crate::rtp;

pub const FRAME: usize = (SAMPLE_RATE / 100) as usize; // 10 ms at 48 kHz
const ENCODE_FRAME: usize = 2 * FRAME; // send 20 ms Opus packets
const VOICE_IDLE: Duration = Duration::from_secs(10);

/// What to do with the game audio stream.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum GameMode {
    /// Viewer: play it, and use it as echo reference.
    Play,
    /// Streamer: the game is already audible locally; only use it as echo
    /// reference so the mic doesn't pick it up and send it to everyone.
    Reference,
    /// Ignore it.
    Off,
}

#[derive(Debug, Clone)]
pub struct EngineConfig {
    pub aec: bool,
    pub noise_suppression: bool,
    pub agc: bool,
    pub game_mode: GameMode,
    pub bitrate: i32,
    pub ssrc: u32,
    /// Test aid: add `gain` × the render signal, `delay_ms` later, to the mic,
    /// like a speaker leaking into the microphone.
    pub echo_sim: Option<(f32, usize)>,
    /// Delay the microphone by this much before echo cancellation.
    ///
    /// The canceller can only remove echo that arrives *after* its reference.
    /// On the streamer the reference (game audio) travels loopback capture →
    /// encoder → RTP → here, which is slower than speakers → air → mic, so the
    /// mic must be held back. See `default_mic_delay_ms`.
    pub mic_delay_ms: usize,
}

/// Mic delay that keeps the echo reference ahead of the echo: on the streamer
/// the game-audio reference is ~40–70 ms behind loopback capture, while the
/// echo reaches the mic ~20–40 ms after it, so hold the mic back 80 ms. Other
/// modes play the reference themselves, so it's always first.
pub fn default_mic_delay_ms(mode: GameMode) -> usize {
    match mode {
        GameMode::Reference => 80,
        GameMode::Play | GameMode::Off => 0,
    }
}

#[derive(Default, Debug, Clone, Copy)]
pub struct Stats {
    pub voice_packets: u64,
    pub game_packets: u64,
    pub sent_packets: u64,
    pub speakers: usize,
}

pub struct Engine {
    cfg: EngineConfig,
    apm: AudioProcessing,
    enc: Encoder,
    voices: HashMap<u32, JitterBuffer>,
    game: Option<JitterBuffer>,
    muted: bool,
    mic_acc: Vec<f32>,
    seq: u16,
    ts: u32,
    echo_line: VecDeque<f32>,
    mic_line: VecDeque<f32>,
    // scratch
    voice_mix: Vec<f32>,
    voice_frame: Vec<f32>,
    game_frame: Vec<f32>,
    reference: Vec<f32>,
    render_out: Vec<f32>,
    mic_in: Vec<f32>,
    mic_out: Vec<f32>,
    packet: Vec<u8>,
    encoded: Vec<u8>,
    pub stats: Stats,
}

impl Engine {
    pub fn new(cfg: EngineConfig) -> Result<Self> {
        let config = Config {
            echo_canceller: cfg.aec.then(EchoCanceller::default),
            noise_suppression: cfg.noise_suppression.then(NoiseSuppression::default),
            gain_controller2: cfg.agc.then(GainController2::default),
            ..Default::default()
        };
        let mono = StreamConfig::new(SAMPLE_RATE, 1);
        let apm = AudioProcessing::builder()
            .config(config)
            .capture_config(mono)
            .render_config(mono)
            .build();
        let enc = Encoder::new_voice(1, cfg.bitrate)?;
        let echo_len = cfg
            .echo_sim
            .map_or(0, |(_, ms)| ms * SAMPLE_RATE as usize / 1000);
        Ok(Engine {
            apm,
            enc,
            voices: HashMap::new(),
            game: None,
            muted: false,
            mic_acc: Vec::with_capacity(ENCODE_FRAME * 2),
            seq: rand_u16(),
            ts: rand_u16() as u32 * 7919,
            echo_line: VecDeque::from(vec![0.0; echo_len]),
            mic_line: VecDeque::from(vec![0.0; cfg.mic_delay_ms * SAMPLE_RATE as usize / 1000]),
            voice_mix: vec![0.0; FRAME],
            voice_frame: vec![0.0; FRAME],
            game_frame: vec![0.0; FRAME * 2],
            reference: vec![0.0; FRAME],
            render_out: vec![0.0; FRAME],
            mic_in: vec![0.0; FRAME],
            mic_out: vec![0.0; FRAME],
            packet: Vec::with_capacity(1500),
            encoded: vec![0; 1500],
            stats: Stats::default(),
            cfg,
        })
    }

    pub fn set_muted(&mut self, muted: bool) {
        self.muted = muted;
    }

    /// A voice packet from another participant (RTP, keyed by SSRC).
    pub fn on_voice_packet(&mut self, buf: &[u8]) {
        let Some(p) = rtp::parse(buf) else { return };
        self.stats.voice_packets += 1;
        let jb = match self.voices.entry(p.ssrc) {
            std::collections::hash_map::Entry::Occupied(e) => e.into_mut(),
            std::collections::hash_map::Entry::Vacant(e) => match Decoder::new(1) {
                Ok(d) => e.insert(JitterBuffer::new(d)),
                Err(_) => return,
            },
        };
        jb.push(p.seq, p.payload);
    }

    /// A game-audio packet (stereo Opus). SSRC is ignored: after a supernode
    /// failover it changes, but sequence numbers continue from the streamer.
    pub fn on_game_packet(&mut self, buf: &[u8]) {
        if self.cfg.game_mode == GameMode::Off {
            return;
        }
        let Some(p) = rtp::parse(buf) else { return };
        self.stats.game_packets += 1;
        if self.game.is_none() {
            // As an echo reference it must arrive as early as possible, so
            // don't wait for a jitter cushion in Reference mode.
            let start = match self.cfg.game_mode {
                GameMode::Reference => 1,
                _ => START_PACKETS,
            };
            match Decoder::new(2) {
                Ok(d) => self.game = Some(JitterBuffer::with_start(d, start)),
                Err(_) => return,
            }
        }
        self.game.as_mut().unwrap().push(p.seq, p.payload);
    }

    /// Processes one 10 ms frame.
    ///
    /// * `mic`: FRAME mono samples from the microphone.
    /// * `out`: receives FRAME stereo (interleaved) samples to play.
    /// * `delay_ms`: estimated playback + capture latency (echo path hint).
    /// * `send`: called with each finished RTP packet of mic audio.
    pub fn tick(
        &mut self,
        mic: &[f32],
        out: &mut [f32],
        delay_ms: i32,
        mut send: impl FnMut(&[u8]),
    ) {
        // 1. Voices: mix every active speaker (mono).
        self.voices
            .retain(|_, jb| jb.last_rx.elapsed() < VOICE_IDLE);
        self.stats.speakers = self.voices.len();
        self.voice_mix.fill(0.0);
        for jb in self.voices.values_mut() {
            if jb.pull(&mut self.voice_frame) {
                for (m, s) in self.voice_mix.iter_mut().zip(&self.voice_frame) {
                    *m += s;
                }
            }
        }

        // 2. Game audio (stereo).
        let game = match self.game.as_mut() {
            Some(jb) => jb.pull(&mut self.game_frame),
            None => false,
        };
        if !game {
            self.game_frame.fill(0.0);
        }

        // 3. Playback mix and the echo reference (everything the speakers emit).
        let play_game = self.cfg.game_mode == GameMode::Play;
        for i in 0..FRAME {
            let (gl, gr) = (self.game_frame[2 * i], self.game_frame[2 * i + 1]);
            let v = self.voice_mix[i];
            let (l, r) = if play_game { (gl + v, gr + v) } else { (v, v) };
            let (l, r) = (l.clamp(-1.0, 1.0), r.clamp(-1.0, 1.0));
            out[2 * i] = l;
            out[2 * i + 1] = r;
            // The reference is what the speakers actually emit, clipping
            // included. In Reference mode the game plays through the OS, so it
            // is added on top of our (voice-only) output.
            self.reference[i] = if play_game {
                (l + r) * 0.5
            } else {
                (l + r) * 0.5 + (gl + gr) * 0.5
            };
        }
        let _ = self
            .apm
            .process_render_f32(&[&self.reference], &mut [&mut self.render_out]);

        // 4. Microphone, plus simulated speaker leakage in tests.
        self.mic_in.copy_from_slice(&mic[..FRAME]);
        if let Some((gain, _)) = self.cfg.echo_sim {
            for i in 0..FRAME {
                self.echo_line.push_back(self.reference[i]);
                let delayed = self.echo_line.pop_front().unwrap_or(0.0);
                self.mic_in[i] += gain * delayed;
            }
        }
        if !self.mic_line.is_empty() {
            for s in self.mic_in.iter_mut() {
                self.mic_line.push_back(*s);
                *s = self.mic_line.pop_front().unwrap_or(0.0);
            }
        }
        let _ = self.apm.set_stream_delay_ms(delay_ms);
        let _ = self
            .apm
            .process_capture_f32(&[&self.mic_in], &mut [&mut self.mic_out]);

        // 5. Encode 20 ms packets and send.
        self.mic_acc.extend_from_slice(&self.mic_out);
        while self.mic_acc.len() >= ENCODE_FRAME {
            if !self.muted
                && let Ok(n) = self
                    .enc
                    .encode(&self.mic_acc[..ENCODE_FRAME], &mut self.encoded)
            {
                rtp::build(
                    &mut self.packet,
                    rtp::OPUS_PAYLOAD_TYPE,
                    self.seq,
                    self.ts,
                    self.cfg.ssrc,
                    &self.encoded[..n],
                );
                send(&self.packet);
                self.stats.sent_packets += 1;
            }
            self.seq = self.seq.wrapping_add(1);
            self.ts = self.ts.wrapping_add(ENCODE_FRAME as u32);
            self.mic_acc.drain(..ENCODE_FRAME);
        }
    }
}

/// Non-cryptographic randomness for RTP sequence/timestamp/SSRC starts.
pub fn rand_u32() -> u32 {
    use std::hash::{BuildHasher, Hasher};
    let mut h = std::collections::hash_map::RandomState::new().build_hasher();
    h.write_u128(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos(),
    );
    h.finish() as u32
}

fn rand_u16() -> u16 {
    rand_u32() as u16
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tone(n: usize, offset: usize, freq: f32, amp: f32) -> Vec<f32> {
        (0..n)
            .map(|i| {
                (2.0 * std::f32::consts::PI * freq * (offset + i) as f32 / SAMPLE_RATE as f32).sin()
                    * amp
            })
            .collect()
    }

    /// Energy of `freq` in `x` (Goertzel), normalised by length.
    pub fn tone_power(x: &[f32], freq: f32) -> f32 {
        let w = 2.0 * std::f32::consts::PI * freq / SAMPLE_RATE as f32;
        let coeff = 2.0 * w.cos();
        let (mut s1, mut s2) = (0.0f32, 0.0f32);
        for &v in x {
            let s = v + coeff * s1 - s2;
            s2 = s1;
            s1 = s;
        }
        (s1 * s1 + s2 * s2 - coeff * s1 * s2) / (x.len() as f32).powi(2)
    }

    /// Runs a far-end talker (440 Hz) into an engine whose mic hears its own
    /// speaker (echo_sim), and returns the 440 Hz power in what it sends back.
    fn echo_sent_back(aec: bool) -> f32 {
        let cfg = EngineConfig {
            aec,
            noise_suppression: false,
            agc: false,
            game_mode: GameMode::Play,
            bitrate: 32_000,
            ssrc: 1,
            echo_sim: Some((0.5, 30)),
            mic_delay_ms: 0,
        };
        let mut eng = Engine::new(cfg).unwrap();
        let mut far = Encoder::new_voice(1, 32_000).unwrap();
        let mut dec = Decoder::new(1).unwrap();
        let mut pkt = Vec::new();
        let mut enc_buf = [0u8; 1500];
        let mut out = vec![0.0; FRAME * 2];
        let silence = vec![0.0; FRAME];
        let mut sent_pcm = Vec::new();
        let mut decoded = vec![0.0f32; 960];

        let seconds = 6;
        for t in 0..(seconds * 100) {
            if t % 2 == 0 {
                // Far-end talker: a 20 ms packet every other tick.
                let pcm = tone(960, t * FRAME, 440.0, 0.4);
                let n = far.encode(&pcm, &mut enc_buf).unwrap();
                rtp::build(
                    &mut pkt,
                    111,
                    (t / 2) as u16,
                    (t * FRAME) as u32,
                    99,
                    &enc_buf[..n],
                );
                eng.on_voice_packet(&pkt);
            }
            eng.tick(&silence, &mut out, 0, |p| {
                let p = rtp::parse(p).unwrap();
                let n = dec.decode(Some(p.payload), &mut decoded).unwrap();
                sent_pcm.extend_from_slice(&decoded[..n]);
            });
        }
        // Measure the last 2 seconds, after the canceller has converged.
        let tail = &sent_pcm[sent_pcm.len() - 2 * SAMPLE_RATE as usize..];
        tone_power(tail, 440.0)
    }

    #[test]
    fn echo_canceller_removes_far_end_from_mic() {
        let without = echo_sent_back(false);
        let with = echo_sent_back(true);
        let erle_db = 10.0 * (without / with.max(1e-12)).log10();
        eprintln!(
            "echo 440 Hz power: without AEC {without:.3e}, with AEC {with:.3e}, reduction {erle_db:.1} dB"
        );
        assert!(without > 1e-4, "echo should be clearly audible without AEC");
        assert!(erle_db > 20.0, "AEC reduced echo by only {erle_db:.1} dB");
    }

    #[test]
    fn mixes_game_and_voice_and_respects_mute() {
        let mut eng = Engine::new(EngineConfig {
            aec: false,
            noise_suppression: false,
            agc: false,
            game_mode: GameMode::Play,
            bitrate: 32_000,
            ssrc: 7,
            echo_sim: None,
            mic_delay_ms: 0,
        })
        .unwrap();
        let mut stereo = Encoder::new_voice(2, 64_000).unwrap();
        let mut buf = [0u8; 1500];
        let mut pkt = Vec::new();
        for s in 0..4u16 {
            let pcm: Vec<f32> = tone(960, s as usize * 960, 300.0, 0.3)
                .into_iter()
                .flat_map(|v| [v, v])
                .collect();
            let n = stereo.encode(&pcm, &mut buf).unwrap();
            rtp::build(&mut pkt, 111, s, s as u32 * 960, 5, &buf[..n]);
            eng.on_game_packet(&pkt);
        }
        let mic = tone(FRAME, 0, 200.0, 0.3);
        let mut out = vec![0.0; FRAME * 2];
        let mut sent = 0;
        for _ in 0..6 {
            eng.tick(&mic, &mut out, 0, |_| sent += 1);
        }
        assert!(
            out.iter().any(|s| s.abs() > 0.05),
            "game audio should be audible"
        );
        assert_eq!(sent, 3, "60 ms of mic = three 20 ms packets");
        eng.set_muted(true);
        for _ in 0..4 {
            eng.tick(&mic, &mut out, 0, |_| sent += 1);
        }
        assert_eq!(sent, 3, "muted: nothing sent");
    }

    /// The streamer's situation: game audio hits the mic `lead_ms` *before*
    /// the same audio reaches the engine as echo reference (it travels
    /// loopback → encoder → RTP). Uses noise: a periodic tone could be
    /// "cancelled" even with the timing wrong. Returns the power of what the
    /// streamer would send.
    fn streamer_echo_sent(aec: bool, mic_delay_ms: usize, lead_ms: usize) -> f32 {
        let mut eng = Engine::new(EngineConfig {
            aec,
            noise_suppression: false,
            agc: false,
            game_mode: GameMode::Reference,
            bitrate: 32_000,
            ssrc: 3,
            echo_sim: None,
            mic_delay_ms,
        })
        .unwrap();
        let seconds = 8;
        let lead = lead_ms * SAMPLE_RATE as usize / 1000;
        let mut seed = 12345u32;
        let noise: Vec<f32> = (0..seconds * SAMPLE_RATE as usize + lead + 960)
            .map(|_| {
                seed = seed.wrapping_mul(1_664_525).wrapping_add(1_013_904_223);
                (seed >> 8) as f32 / (1u32 << 24) as f32 * 0.6 - 0.3
            })
            .collect();
        let mut game_enc = Encoder::new_voice(2, 96_000).unwrap();
        let mut dec = Decoder::new(1).unwrap();
        let (mut pkt, mut buf, mut out) = (Vec::new(), [0u8; 1500], vec![0.0; FRAME * 2]);
        let mut decoded = vec![0.0f32; 960];
        let mut sent = Vec::new();
        for t in 0..seconds * 100 {
            if t % 2 == 0 {
                let k = t / 2;
                let stereo: Vec<f32> = noise[k * 960..(k + 1) * 960]
                    .iter()
                    .flat_map(|&v| [v, v])
                    .collect();
                let n = game_enc.encode(&stereo, &mut buf).unwrap();
                rtp::build(&mut pkt, 111, k as u16, (k * 960) as u32, 9, &buf[..n]);
                eng.on_game_packet(&pkt);
            }
            // Echo: the same game sound, `lead` samples ahead of the reference.
            let mic: Vec<f32> = noise[t * FRAME + lead..t * FRAME + lead + FRAME]
                .iter()
                .map(|v| v * 0.5)
                .collect();
            eng.tick(&mic, &mut out, 0, |p| {
                let p = rtp::parse(p).unwrap();
                let n = dec.decode(Some(p.payload), &mut decoded).unwrap();
                sent.extend_from_slice(&decoded[..n]);
            });
        }
        let tail = &sent[sent.len() - 2 * SAMPLE_RATE as usize..];
        tail.iter().map(|s| s * s).sum::<f32>() / tail.len() as f32
    }

    #[test]
    fn streamer_mic_delay_lets_aec_cancel_game_audio_that_leads_its_reference() {
        let lead = 50;
        let leaked = streamer_echo_sent(false, 0, lead);
        let without_delay = streamer_echo_sent(true, 0, lead);
        let with_delay = streamer_echo_sent(true, default_mic_delay_ms(GameMode::Reference), lead);
        let db = |a: f32, b: f32| 10.0 * (a / b.max(1e-12)).log10();
        eprintln!(
            "game echo leading its reference by {lead} ms: leaked {leaked:.2e}; AEC without mic delay \
             {without_delay:.2e} ({:.1} dB); AEC with {} ms mic delay {with_delay:.2e} ({:.1} dB)",
            db(leaked, without_delay),
            default_mic_delay_ms(GameMode::Reference),
            db(leaked, with_delay),
        );
        assert!(
            db(leaked, with_delay) > 20.0,
            "with mic delay, echo reduced only {:.1} dB",
            db(leaked, with_delay)
        );
        assert!(
            db(without_delay, with_delay) > 10.0,
            "mic delay should clearly beat no delay ({:.1} dB)",
            db(without_delay, with_delay)
        );
    }

    #[test]
    fn echo_reference_matches_clipped_speaker_output() {
        let mut eng = Engine::new(EngineConfig {
            aec: false,
            noise_suppression: false,
            agc: false,
            game_mode: GameMode::Play,
            bitrate: 32_000,
            ssrc: 1,
            echo_sim: None,
            mic_delay_ms: 0,
        })
        .unwrap();
        // Two loud voices that sum past full scale.
        let mut buf = [0u8; 1500];
        let mut pkt = Vec::new();
        for ssrc in [1u32, 2] {
            let mut enc = Encoder::new_voice(1, 64_000).unwrap();
            for s in 0..4u16 {
                let n = enc.encode(&vec![0.9f32; 960], &mut buf).unwrap();
                rtp::build(&mut pkt, 111, s, s as u32 * 960, ssrc, &buf[..n]);
                eng.on_voice_packet(&pkt);
            }
        }
        let mut out = vec![0.0; FRAME * 2];
        for _ in 0..4 {
            eng.tick(&vec![0.0; FRAME], &mut out, 0, |_| {});
            for i in 0..FRAME {
                let speakers = (out[2 * i] + out[2 * i + 1]) * 0.5;
                assert!(
                    (eng.reference[i] - speakers).abs() < 1e-6,
                    "reference must equal what the speakers play"
                );
                assert!(eng.reference[i].abs() <= 1.0);
            }
        }
    }
}
