// voice-worklet.js — runs in the AudioWorklet realm. Pulls 128-frame
// blocks from the mic graph (always native AudioContext sample rate,
// usually 48 kHz), downsamples to 16 kHz mono, converts Float32 to int16
// little-endian PCM, and posts ArrayBuffers to the main thread.
//
// The downsampler is a simple decimating averager: take a window of
// ratio samples, output their average. Good enough for voice; we don't
// pull in a proper polyphase filter because the size cost in a worklet
// bundle isn't worth the 0.x dB SNR gain on a model that's going to
// re-resample anyway.

const TARGET_RATE = 16000;

class VoiceCaptureProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.ratio = sampleRate / TARGET_RATE; // sampleRate is a worklet global
    this.accumulator = 0;
    this.acc = 0;
    this.accCount = 0;
    // ~20 ms at 16 kHz = 320 samples = 640 bytes. Buffer up to that
    // before posting so we don't drown the main thread with tiny
    // messages.
    this.outBuf = new Int16Array(320);
    this.outIdx = 0;
  }

  process(inputs) {
    const input = inputs[0];
    if (!input || input.length === 0) return true;
    const ch = input[0];
    if (!ch || ch.length === 0) return true;

    for (let i = 0; i < ch.length; i++) {
      this.acc += ch[i];
      this.accCount += 1;
      this.accumulator += 1;
      if (this.accumulator >= this.ratio) {
        const avg = this.acc / this.accCount;
        // Float32 [-1, 1] → int16 little-endian
        let s = Math.max(-1, Math.min(1, avg));
        s = s < 0 ? s * 0x8000 : s * 0x7fff;
        this.outBuf[this.outIdx++] = s | 0;
        this.acc = 0;
        this.accCount = 0;
        this.accumulator -= this.ratio;
        if (this.outIdx >= this.outBuf.length) {
          // Transfer a fresh ArrayBuffer copy; the receiver may hold it.
          const copy = new Int16Array(this.outBuf);
          this.port.postMessage(copy.buffer, [copy.buffer]);
          this.outIdx = 0;
        }
      }
    }
    return true;
  }
}

registerProcessor('voice-capture', VoiceCaptureProcessor);
