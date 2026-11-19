//! Audio input/output: real devices through cpal, or synthetic sources and
//! file/null sinks for tests and diagnosis. Everything is converted to the
//! engine's format here: 48 kHz, mono in, stereo out.

use std::fs::File;
use std::io::{BufWriter, Write};

use anyhow::{Context, Result, bail};
use cpal::traits::{DeviceTrait, HostTrait, StreamTrait};
use cpal::{SampleFormat, StreamConfig};
use ringbuf::traits::{Consumer, Observer, Producer, Split};
use ringbuf::{HeapCons, HeapProd, HeapRb};

use crate::engine::FRAME;
use crate::opus::SAMPLE_RATE;
use crate::resample::{Resampler, from_stereo, to_mono};

pub enum Input {
    Device {
        cons: HeapCons<f32>,
        _stream: cpal::Stream,
    },
    Sine {
        freq: f32,
        n: usize,
    },
    Silence,
}

impl Input {
    /// `default` (system microphone), `sine:FREQ` or `silence`.
    pub fn open(spec: &str) -> Result<Input> {
        if let Some(f) = spec.strip_prefix("sine:") {
            return Ok(Input::Sine {
                freq: f.parse().context("sine frequency")?,
                n: 0,
            });
        }
        match spec {
            "silence" => Ok(Input::Silence),
            "default" => open_input_device(),
            _ => bail!("unknown input {spec:?} (default | sine:FREQ | silence)"),
        }
    }

    pub fn is_device(&self) -> bool {
        matches!(self, Input::Device { .. })
    }

    /// Fills one 10 ms mono frame. A device returns false until enough audio
    /// has been captured; synthetic inputs always succeed.
    pub fn read(&mut self, frame: &mut [f32]) -> bool {
        match self {
            Input::Device { cons, .. } => {
                if cons.occupied_len() < FRAME {
                    return false;
                }
                cons.pop_slice(&mut frame[..FRAME]);
                true
            }
            Input::Sine { freq, n } => {
                for s in frame.iter_mut().take(FRAME) {
                    *s = (2.0 * std::f32::consts::PI * *freq * *n as f32 / SAMPLE_RATE as f32)
                        .sin()
                        * 0.3;
                    *n = (*n + 1) % (SAMPLE_RATE as usize * 100);
                }
                true
            }
            Input::Silence => {
                frame[..FRAME].fill(0.0);
                true
            }
        }
    }

    pub fn buffered_ms(&self) -> i32 {
        match self {
            Input::Device { cons, .. } => {
                (cons.occupied_len() * 1000 / SAMPLE_RATE as usize) as i32
            }
            _ => 0,
        }
    }
}

fn open_input_device() -> Result<Input> {
    let dev = cpal::default_host()
        .default_input_device()
        .context("no microphone found")?;
    let sup = dev.default_input_config()?;
    let (ch, rate) = (sup.channels() as usize, sup.sample_rate());
    let config = StreamConfig {
        channels: sup.channels(),
        sample_rate: rate,
        buffer_size: cpal::BufferSize::Default,
    };
    let (mut prod, cons) = HeapRb::<f32>::new(SAMPLE_RATE as usize / 2).split(); // 500 ms
    let mut rs = Resampler::new(1, rate, SAMPLE_RATE);
    let (mut mono, mut res) = (Vec::new(), Vec::new());
    let mut handle = move |data: &[f32]| {
        mono.clear();
        to_mono(data, ch, &mut mono);
        res.clear();
        rs.process(&mono, &mut res);
        prod.push_slice(&res); // if the engine stalls, excess is dropped
    };
    let on_err = |e| eprintln!("voice: microphone error: {e}");
    let stream = match sup.sample_format() {
        SampleFormat::F32 => dev.build_input_stream::<f32, _, _>(
            config,
            move |d: &[f32], _| handle(d),
            on_err,
            None,
        )?,
        SampleFormat::I16 => {
            let mut conv = Vec::new();
            dev.build_input_stream::<i16, _, _>(
                config,
                move |d: &[i16], _| {
                    conv.clear();
                    conv.extend(d.iter().map(|&s| s as f32 / 32768.0));
                    handle(&conv);
                },
                on_err,
                None,
            )?
        }
        other => bail!("unsupported microphone sample format {other:?}"),
    };
    stream.play()?;
    eprintln!(
        "voice: microphone {:?} ({ch} ch, {rate} Hz)",
        dev.description()
            .map(|d| d.name().to_owned())
            .unwrap_or_default()
    );
    Ok(Input::Device {
        cons,
        _stream: stream,
    })
}

pub enum Output {
    Device {
        prod: HeapProd<f32>,
        ch: usize,
        rate: u32,
        rs: Resampler,
        mapped: Vec<f32>,
        resampled: Vec<f32>,
        _stream: cpal::Stream,
    },
    /// Raw 32-bit float, little-endian, 48 kHz stereo interleaved.
    Raw(BufWriter<File>),
    Null,
}

/// Playback buffering above which frames are dropped (keeps latency bounded
/// when the device clock runs slower than the input clock).
const MAX_OUTPUT_MS: usize = 120;

impl Output {
    /// `default` (system speakers), `raw:PATH` or `null`.
    pub fn open(spec: &str) -> Result<Output> {
        if let Some(path) = spec.strip_prefix("raw:") {
            return Ok(Output::Raw(BufWriter::new(
                File::create(path).with_context(|| format!("create {path}"))?,
            )));
        }
        match spec {
            "null" => Ok(Output::Null),
            "default" => open_output_device(),
            _ => bail!("unknown output {spec:?} (default | raw:PATH | null)"),
        }
    }

    pub fn write(&mut self, stereo: &[f32]) {
        match self {
            Output::Device {
                prod,
                ch,
                rate,
                rs,
                mapped,
                resampled,
                ..
            } => {
                mapped.clear();
                from_stereo(stereo, *ch, mapped);
                resampled.clear();
                rs.process(mapped, resampled);
                if prod.occupied_len() < *rate as usize * *ch * MAX_OUTPUT_MS / 1000 {
                    prod.push_slice(resampled);
                }
            }
            Output::Raw(w) => {
                for s in stereo {
                    let _ = w.write_all(&s.to_le_bytes());
                }
            }
            Output::Null => {}
        }
    }

    pub fn flush(&mut self) {
        if let Output::Raw(w) = self {
            let _ = w.flush();
        }
    }

    pub fn buffered_ms(&self) -> i32 {
        match self {
            Output::Device { prod, ch, rate, .. } => {
                (prod.occupied_len() * 1000 / (*rate as usize * *ch)) as i32
            }
            _ => 0,
        }
    }
}

fn open_output_device() -> Result<Output> {
    let dev = cpal::default_host()
        .default_output_device()
        .context("no speakers found")?;
    let sup = dev.default_output_config()?;
    let (ch, rate) = (sup.channels() as usize, sup.sample_rate());
    let config = StreamConfig {
        channels: sup.channels(),
        sample_rate: rate,
        buffer_size: cpal::BufferSize::Default,
    };
    let (prod, mut cons) = HeapRb::<f32>::new(rate as usize * ch / 2).split(); // 500 ms
    let on_err = |e| eprintln!("voice: speaker error: {e}");
    let stream = match sup.sample_format() {
        SampleFormat::F32 => dev.build_output_stream::<f32, _, _>(
            config,
            move |d: &mut [f32], _| {
                let n = cons.pop_slice(d);
                d[n..].fill(0.0); // underrun: silence
            },
            on_err,
            None,
        )?,
        SampleFormat::I16 => {
            let mut tmp = Vec::new();
            dev.build_output_stream::<i16, _, _>(
                config,
                move |d: &mut [i16], _| {
                    tmp.resize(d.len(), 0.0);
                    let n = cons.pop_slice(&mut tmp);
                    tmp[n..].fill(0.0);
                    for (o, s) in d.iter_mut().zip(&tmp) {
                        *o = (s.clamp(-1.0, 1.0) * 32767.0) as i16;
                    }
                },
                on_err,
                None,
            )?
        }
        other => bail!("unsupported speaker sample format {other:?}"),
    };
    stream.play()?;
    eprintln!(
        "voice: speakers {:?} ({ch} ch, {rate} Hz)",
        dev.description()
            .map(|d| d.name().to_owned())
            .unwrap_or_default()
    );
    Ok(Output::Device {
        prod,
        ch,
        rate,
        rs: Resampler::new(ch, SAMPLE_RATE, rate),
        mapped: Vec::new(),
        resampled: Vec::new(),
        _stream: stream,
    })
}

/// Prints the default devices and their formats (no audio is opened).
pub fn list_devices() -> Result<()> {
    let host = cpal::default_host();
    for (kind, dev) in [
        ("input", host.default_input_device()),
        ("output", host.default_output_device()),
    ] {
        match dev {
            None => println!("{kind}: none"),
            Some(d) => {
                let name = d
                    .description()
                    .map(|x| x.name().to_owned())
                    .unwrap_or_default();
                let cfg = if kind == "input" {
                    d.default_input_config()
                } else {
                    d.default_output_config()
                };
                match cfg {
                    Ok(c) => println!(
                        "{kind}: {name} ({} ch, {} Hz, {:?})",
                        c.channels(),
                        c.sample_rate(),
                        c.sample_format()
                    ),
                    Err(e) => println!("{kind}: {name} (config error: {e})"),
                }
            }
        }
    }
    Ok(())
}
