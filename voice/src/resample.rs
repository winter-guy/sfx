//! Linear resampler and channel conversion between the device's format and
//! the engine's (48 kHz; mono capture, stereo playback). Linear interpolation
//! is plenty for voice; devices usually run at 48 kHz anyway (no-op path).

pub struct Resampler {
    ch: usize,
    step: f64, // input frames per output frame
    pos: f64,
    buf: Vec<f32>,
}

impl Resampler {
    pub fn new(channels: usize, in_rate: u32, out_rate: u32) -> Self {
        Resampler {
            ch: channels,
            step: in_rate as f64 / out_rate as f64,
            pos: 0.0,
            buf: Vec::new(),
        }
    }

    /// Appends resampled frames for `input` (interleaved) to `out`.
    pub fn process(&mut self, input: &[f32], out: &mut Vec<f32>) {
        if self.step == 1.0 {
            out.extend_from_slice(input);
            return;
        }
        let ch = self.ch;
        self.buf.extend_from_slice(input);
        let frames = self.buf.len() / ch;
        while self.pos + 1.0 < frames as f64 {
            let i = self.pos as usize;
            let f = (self.pos - i as f64) as f32;
            for c in 0..ch {
                let a = self.buf[i * ch + c];
                let b = self.buf[(i + 1) * ch + c];
                out.push(a + (b - a) * f);
            }
            self.pos += self.step;
        }
        let consumed = (self.pos as usize).min(frames);
        self.buf.drain(..consumed * ch);
        self.pos -= consumed as f64;
    }
}

/// Averages interleaved channels into mono.
pub fn to_mono(input: &[f32], channels: usize, out: &mut Vec<f32>) {
    if channels == 1 {
        out.extend_from_slice(input);
        return;
    }
    out.extend(
        input
            .chunks_exact(channels)
            .map(|f| f.iter().sum::<f32>() / channels as f32),
    );
}

/// Maps interleaved stereo to `channels` (mono = average, extra channels silent).
pub fn from_stereo(input: &[f32], channels: usize, out: &mut Vec<f32>) {
    match channels {
        2 => out.extend_from_slice(input),
        1 => out.extend(input.as_chunks::<2>().0.iter().map(|f| (f[0] + f[1]) * 0.5)),
        n => {
            for f in input.as_chunks::<2>().0 {
                out.extend_from_slice(f);
                out.extend(std::iter::repeat_n(0.0, n - 2));
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn resamples_44k1_to_48k_with_right_length_and_shape() {
        let mut r = Resampler::new(1, 44_100, 48_000);
        let input: Vec<f32> = (0..44_100)
            .map(|i| (i as f32 / 44_100.0 * 2.0 * std::f32::consts::PI * 100.0).sin())
            .collect();
        let mut out = Vec::new();
        for chunk in input.chunks(441) {
            r.process(chunk, &mut out);
        }
        assert!((out.len() as i64 - 48_000).abs() <= 2, "got {}", out.len());
        // Still a 100 Hz sine: compare against the ideal at 48 kHz.
        let err: f32 = out
            .iter()
            .enumerate()
            .map(|(i, s)| {
                (s - (i as f32 / 48_000.0 * 2.0 * std::f32::consts::PI * 100.0).sin()).abs()
            })
            .fold(0.0, f32::max);
        assert!(err < 0.01, "max error {err}");
    }

    #[test]
    fn channel_mapping() {
        let mut m = Vec::new();
        to_mono(&[1.0, 0.0, 0.5, 0.5], 2, &mut m);
        assert_eq!(m, vec![0.5, 0.5]);
        let mut s = Vec::new();
        from_stereo(&[1.0, 0.0], 4, &mut s);
        assert_eq!(s, vec![1.0, 0.0, 0.0, 0.0]);
        let mut one = Vec::new();
        from_stereo(&[1.0, 0.0], 1, &mut one);
        assert_eq!(one, vec![0.5]);
    }
}
