//! Per-stream jitter buffer: reorders Opus packets by sequence number,
//! decodes them, conceals losses and hands out fixed 10 ms frames.

use std::collections::{BTreeMap, VecDeque};
use std::time::Instant;

use crate::opus::Decoder;

/// Packets buffered before playback (re)starts: absorbs network jitter.
const START_PACKETS: usize = 2;
/// Above this, drop the oldest packets so latency can't grow without bound
/// (e.g. clock drift or a burst after a stall). 10 × 20 ms = 200 ms.
const MAX_PACKETS: usize = 10;
/// Gaps up to this many packets are concealed; larger ones are skipped.
const MAX_CONCEAL: u64 = 5;
/// Largest Opus packet: 120 ms at 48 kHz.
const MAX_FRAME: usize = 5760;

pub struct JitterBuffer {
    dec: Decoder,
    ch: usize,
    packets: BTreeMap<u64, Vec<u8>>,
    highest: Option<u64>,
    next: Option<u64>,
    pcm: VecDeque<f32>,
    playing: bool,
    last_frame: usize, // samples per channel of the last decoded packet
    decode_buf: Vec<f32>,
    pub last_rx: Instant,
    pub stats: Stats,
}

#[derive(Default, Debug, Clone, Copy)]
pub struct Stats {
    pub received: u64,
    pub late: u64,
    pub concealed: u64,
    pub dropped: u64,
    pub bad: u64,
}

impl JitterBuffer {
    pub fn new(dec: Decoder) -> Self {
        let ch = dec.channels();
        JitterBuffer {
            dec,
            ch,
            packets: BTreeMap::new(),
            highest: None,
            next: None,
            pcm: VecDeque::new(),
            playing: false,
            last_frame: 960,
            decode_buf: vec![0.0; MAX_FRAME * ch],
            last_rx: Instant::now(),
            stats: Stats::default(),
        }
    }

    /// Extends a 16-bit sequence number to 64 bits, choosing the value
    /// closest to the highest seen so far (handles wrap-around both ways).
    fn unwrap_seq(&self, seq: u16) -> u64 {
        let Some(h) = self.highest else {
            return (1u64 << 32) + seq as u64;
        };
        let base = (h & !0xFFFF) | seq as u64;
        [base.wrapping_sub(1 << 16), base, base + (1 << 16)]
            .into_iter()
            .min_by_key(|c| c.abs_diff(h))
            .unwrap()
    }

    pub fn push(&mut self, seq: u16, payload: &[u8]) {
        self.last_rx = Instant::now();
        self.stats.received += 1;
        let ext = self.unwrap_seq(seq);
        if self.next.is_some_and(|n| ext < n) {
            self.stats.late += 1;
            return;
        }
        self.highest = Some(self.highest.map_or(ext, |h| h.max(ext)));
        self.packets.insert(ext, payload.to_vec());
        while self.packets.len() > MAX_PACKETS {
            self.packets.pop_first();
            self.stats.dropped += 1;
            self.next = self.packets.keys().next().copied();
        }
    }

    fn decode_into_pcm(&mut self, packet: Option<Vec<u8>>) {
        let samples = match &packet {
            Some(_) => MAX_FRAME,
            None => self.last_frame,
        };
        let buf = &mut self.decode_buf[..samples * self.ch];
        match self.dec.decode(packet.as_deref(), buf) {
            Ok(n) => {
                if packet.is_some() {
                    self.last_frame = n;
                }
                self.pcm.extend(&self.decode_buf[..n * self.ch]);
            }
            Err(_) => self.stats.bad += 1,
        }
    }

    /// Fills `out` (10 ms, interleaved) with the next audio. Returns false and
    /// writes silence while buffering or when the stream has run dry.
    pub fn pull(&mut self, out: &mut [f32]) -> bool {
        if !self.playing {
            if self.packets.len() < START_PACKETS {
                out.fill(0.0);
                return false;
            }
            self.playing = true;
            self.next = self.packets.keys().next().copied();
        }
        while self.pcm.len() < out.len() {
            let Some(n) = self.next else { break };
            if let Some(p) = self.packets.remove(&n) {
                self.decode_into_pcm(Some(p));
                self.next = Some(n + 1);
            } else if let Some(&first) = self.packets.keys().next() {
                if first - n > MAX_CONCEAL {
                    self.next = Some(first); // long gap: skip ahead
                } else {
                    self.decode_into_pcm(None); // short gap: conceal
                    self.stats.concealed += 1;
                    self.next = Some(n + 1);
                }
            } else {
                // Ran dry (speaker stopped, or a long stall): rebuffer.
                self.playing = false;
                break;
            }
        }
        let take = self.pcm.len().min(out.len());
        for (o, s) in out.iter_mut().zip(self.pcm.drain(..take)) {
            *o = s;
        }
        out[take..].fill(0.0);
        take > 0
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::opus::Encoder;

    fn packets(n: usize) -> Vec<Vec<u8>> {
        let mut enc = Encoder::new_voice(1, 32_000).unwrap();
        let mut buf = [0u8; 1500];
        (0..n)
            .map(|f| {
                let pcm: Vec<f32> = (0..960)
                    .map(|i| {
                        (2.0 * std::f32::consts::PI * 440.0 * (f * 960 + i) as f32 / 48_000.0).sin()
                            * 0.5
                    })
                    .collect();
                let len = enc.encode(&pcm, &mut buf).unwrap();
                buf[..len].to_vec()
            })
            .collect()
    }

    fn jb() -> JitterBuffer {
        JitterBuffer::new(Decoder::new(1).unwrap())
    }

    #[test]
    fn buffers_then_plays_in_order_across_seq_wrap() {
        let p = packets(6);
        let mut j = jb();
        let mut out = [0.0f32; 480];
        j.push(65534, &p[0]);
        assert!(!j.pull(&mut out), "should wait for START_PACKETS");
        j.push(65535, &p[1]);
        j.push(0, &p[2]); // wraps
        j.push(1, &p[3]);
        let mut frames = 0;
        while j.pull(&mut out) {
            frames += 1;
        }
        assert_eq!(frames, 8, "4 packets × 20 ms = 8 × 10 ms frames");
        assert_eq!(j.stats.concealed, 0);
    }

    #[test]
    fn reorders_and_conceals_gaps_and_drops_late() {
        let p = packets(6);
        let mut j = jb();
        let mut out = [0.0f32; 480];
        j.push(11, &p[1]);
        j.push(10, &p[0]); // out of order
        j.push(13, &p[3]); // 12 missing
        for _ in 0..8 {
            j.pull(&mut out);
        }
        assert_eq!(j.stats.concealed, 1, "packet 12 concealed");
        j.push(12, &p[2]); // arrives after its slot: late
        assert_eq!(j.stats.late, 1);
    }

    #[test]
    fn caps_latency() {
        let p = packets(1);
        let mut j = jb();
        for s in 0..(MAX_PACKETS as u16 + 5) {
            j.push(s, &p[0]);
        }
        assert_eq!(j.packets.len(), MAX_PACKETS);
        assert_eq!(j.stats.dropped, 5);
    }
}
