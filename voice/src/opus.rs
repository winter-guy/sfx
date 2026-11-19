//! Small safe wrapper over libopus (via opusic-sys).

use std::ffi::CStr;
use std::ptr::NonNull;

use anyhow::{Result, bail};
use opusic_sys as sys;

pub const SAMPLE_RATE: u32 = 48_000;

fn err(code: i32) -> String {
    // SAFETY: opus_strerror returns a static NUL-terminated string.
    unsafe { CStr::from_ptr(sys::opus_strerror(code)) }
        .to_string_lossy()
        .into_owned()
}

/// Opus encoder for voice: VOIP mode, in-band FEC on.
pub struct Encoder {
    st: NonNull<sys::OpusEncoder>,
    channels: usize,
}

// SAFETY: an OpusEncoder has no thread affinity; we only use it via &mut.
unsafe impl Send for Encoder {}

impl Encoder {
    pub fn new_voice(channels: usize, bitrate: i32) -> Result<Self> {
        let mut code = 0;
        // SAFETY: valid arguments; the result is checked below.
        let st = unsafe {
            sys::opus_encoder_create(
                SAMPLE_RATE as i32,
                channels as i32,
                sys::OPUS_APPLICATION_VOIP,
                &mut code,
            )
        };
        let Some(st) = NonNull::new(st).filter(|_| code == sys::OPUS_OK) else {
            bail!("opus_encoder_create: {}", err(code));
        };
        let enc = Encoder { st, channels };
        // SAFETY: valid encoder and request/value pairs from the Opus API.
        unsafe {
            sys::opus_encoder_ctl(enc.st.as_ptr(), sys::OPUS_SET_BITRATE_REQUEST, bitrate);
            sys::opus_encoder_ctl(enc.st.as_ptr(), sys::OPUS_SET_INBAND_FEC_REQUEST, 1i32);
            sys::opus_encoder_ctl(
                enc.st.as_ptr(),
                sys::OPUS_SET_PACKET_LOSS_PERC_REQUEST,
                10i32,
            );
        }
        Ok(enc)
    }

    /// Encodes one frame (`pcm.len() / channels` samples per channel, e.g.
    /// 960 for 20 ms). Returns the packet length written into `out`.
    pub fn encode(&mut self, pcm: &[f32], out: &mut [u8]) -> Result<usize> {
        let frame = (pcm.len() / self.channels) as i32;
        // SAFETY: pcm holds `frame * channels` samples; out is writable.
        let n = unsafe {
            sys::opus_encode_float(
                self.st.as_ptr(),
                pcm.as_ptr(),
                frame,
                out.as_mut_ptr(),
                out.len() as i32,
            )
        };
        if n < 0 {
            bail!("opus_encode_float: {}", err(n));
        }
        Ok(n as usize)
    }
}

impl Drop for Encoder {
    fn drop(&mut self) {
        // SAFETY: created by opus_encoder_create, destroyed once.
        unsafe { sys::opus_encoder_destroy(self.st.as_ptr()) }
    }
}

pub struct Decoder {
    st: NonNull<sys::OpusDecoder>,
    channels: usize,
}

// SAFETY: as for Encoder.
unsafe impl Send for Decoder {}

impl Decoder {
    pub fn new(channels: usize) -> Result<Self> {
        let mut code = 0;
        // SAFETY: valid arguments; the result is checked below.
        let st =
            unsafe { sys::opus_decoder_create(SAMPLE_RATE as i32, channels as i32, &mut code) };
        let Some(st) = NonNull::new(st).filter(|_| code == sys::OPUS_OK) else {
            bail!("opus_decoder_create: {}", err(code));
        };
        Ok(Decoder { st, channels })
    }

    pub fn channels(&self) -> usize {
        self.channels
    }

    /// Decodes `packet` into `out` (interleaved). With `None`, runs packet loss
    /// concealment for `out.len() / channels` samples per channel. Returns the
    /// number of samples per channel produced.
    pub fn decode(&mut self, packet: Option<&[u8]>, out: &mut [f32]) -> Result<usize> {
        let (ptr, len) = match packet {
            Some(p) => (p.as_ptr(), p.len() as i32),
            None => (std::ptr::null(), 0),
        };
        let frame = (out.len() / self.channels) as i32;
        // SAFETY: out holds `frame * channels` samples; a null packet means PLC.
        let n = unsafe {
            sys::opus_decode_float(self.st.as_ptr(), ptr, len, out.as_mut_ptr(), frame, 0)
        };
        if n < 0 {
            bail!("opus_decode_float: {}", err(n));
        }
        Ok(n as usize)
    }
}

impl Drop for Decoder {
    fn drop(&mut self) {
        // SAFETY: created by opus_decoder_create, destroyed once.
        unsafe { sys::opus_decoder_destroy(self.st.as_ptr()) }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn encode_decode_roundtrip_keeps_a_tone() {
        let mut enc = Encoder::new_voice(1, 32_000).unwrap();
        let mut dec = Decoder::new(1).unwrap();
        let mut packet = [0u8; 1500];
        let mut out = vec![0.0f32; 960];
        let mut energy = 0.0;
        for f in 0..20 {
            let pcm: Vec<f32> = (0..960)
                .map(|i| {
                    (2.0 * std::f32::consts::PI * 440.0 * (f * 960 + i) as f32 / 48_000.0).sin()
                        * 0.5
                })
                .collect();
            let n = enc.encode(&pcm, &mut packet).unwrap();
            assert!(n > 0);
            assert_eq!(dec.decode(Some(&packet[..n]), &mut out).unwrap(), 960);
            if f >= 5 {
                energy += out.iter().map(|s| s * s).sum::<f32>();
            }
        }
        assert!(energy / (15.0 * 960.0) > 0.05, "decoded tone too quiet");
        // Packet loss concealment produces a full frame.
        assert_eq!(dec.decode(None, &mut out).unwrap(), 960);
    }
}
