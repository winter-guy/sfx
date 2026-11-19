//! Minimal RTP (RFC 3550) parsing and building: only what Opus over a
//! local UDP hop needs.

pub const OPUS_PAYLOAD_TYPE: u8 = 111;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Packet<'a> {
    pub payload_type: u8,
    pub seq: u16,
    pub timestamp: u32,
    pub ssrc: u32,
    pub payload: &'a [u8],
}

/// Parses an RTP packet. Returns None for anything that isn't RTP version 2,
/// is truncated, has no payload, or is RTCP multiplexed on the same port.
pub fn parse(buf: &[u8]) -> Option<Packet<'_>> {
    if buf.len() < 12 || buf[0] >> 6 != 2 {
        return None;
    }
    let padding = buf[0] & 0x20 != 0;
    let extension = buf[0] & 0x10 != 0;
    let csrc_count = (buf[0] & 0x0f) as usize;
    let payload_type = buf[1] & 0x7f;
    if (72..=76).contains(&payload_type) {
        return None; // RTCP
    }
    let mut start = 12 + 4 * csrc_count;
    if extension {
        if buf.len() < start + 4 {
            return None;
        }
        let words = u16::from_be_bytes([buf[start + 2], buf[start + 3]]) as usize;
        start += 4 + 4 * words;
    }
    let mut end = buf.len();
    if start > end {
        return None;
    }
    if padding {
        let pad = buf[end - 1] as usize;
        if pad == 0 || pad > end - start {
            return None;
        }
        end -= pad;
    }
    if start == end {
        return None;
    }
    Some(Packet {
        payload_type,
        seq: u16::from_be_bytes([buf[2], buf[3]]),
        timestamp: u32::from_be_bytes([buf[4], buf[5], buf[6], buf[7]]),
        ssrc: u32::from_be_bytes([buf[8], buf[9], buf[10], buf[11]]),
        payload: &buf[start..end],
    })
}

/// Writes a minimal RTP packet (no CSRCs, no extensions) into `out`.
pub fn build(
    out: &mut Vec<u8>,
    payload_type: u8,
    seq: u16,
    timestamp: u32,
    ssrc: u32,
    payload: &[u8],
) {
    out.clear();
    out.push(0x80);
    out.push(payload_type & 0x7f);
    out.extend_from_slice(&seq.to_be_bytes());
    out.extend_from_slice(&timestamp.to_be_bytes());
    out.extend_from_slice(&ssrc.to_be_bytes());
    out.extend_from_slice(payload);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roundtrip() {
        let mut buf = Vec::new();
        build(
            &mut buf,
            OPUS_PAYLOAD_TYPE,
            65535,
            960,
            0xDEADBEEF,
            &[1, 2, 3],
        );
        let p = parse(&buf).unwrap();
        assert_eq!(p.payload_type, OPUS_PAYLOAD_TYPE);
        assert_eq!(p.seq, 65535);
        assert_eq!(p.timestamp, 960);
        assert_eq!(p.ssrc, 0xDEADBEEF);
        assert_eq!(p.payload, &[1, 2, 3]);
    }

    #[test]
    fn handles_csrc_extension_and_padding() {
        // V=2, P=1, X=1, CC=1
        let mut buf = vec![0xB1, 111, 0, 7, 0, 0, 0, 1, 0, 0, 0, 2];
        buf.extend_from_slice(&[9, 9, 9, 9]); // CSRC
        buf.extend_from_slice(&[0xBE, 0xDE, 0, 1, 1, 2, 3, 4]); // 1-word extension
        buf.extend_from_slice(&[0xAA, 0xBB]); // payload
        buf.extend_from_slice(&[0, 2]); // 2 bytes padding
        let p = parse(&buf).unwrap();
        assert_eq!(p.seq, 7);
        assert_eq!(p.payload, &[0xAA, 0xBB]);
    }

    #[test]
    fn rejects_junk_and_rtcp() {
        assert!(parse(b"hello").is_none());
        assert!(parse(&[0x40; 20]).is_none()); // version 1
        let mut rtcp = Vec::new();
        build(&mut rtcp, 72, 1, 1, 1, &[1]);
        assert!(parse(&rtcp).is_none());
        let mut empty = Vec::new();
        build(&mut empty, 111, 1, 1, 1, &[]);
        assert!(parse(&empty).is_none());
    }
}
