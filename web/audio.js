let context;
const worklets = new WeakMap();

export function getAudioContext() {
  if (!context || context.state === 'closed') {
    context = new AudioContext({ latencyHint: 'interactive' });
  }
  return context;
}

export function resumeAudio() {
  const ctx = getAudioContext();
  if (ctx.state !== 'running') ctx.resume().catch(() => {});
}

function loadRecordingWorklet(ctx) {
  if (!worklets.has(ctx)) {
    const source = `
      class RecTap extends AudioWorkletProcessor {
        process(inputs) {
          const samples = inputs[0]?.[0];
          if (samples) this.port.postMessage(samples);
          return true;
        }
      }
      registerProcessor('rec-tap', RecTap);
    `;
    const url = URL.createObjectURL(new Blob([source], { type: 'text/javascript' }));
    const ready = ctx.audioWorklet.addModule(url)
      .catch((error) => { worklets.delete(ctx); throw error; })
      .finally(() => URL.revokeObjectURL(url));
    worklets.set(ctx, ready);
  }
  return worklets.get(ctx);
}

export function createRecorder(seconds) {
  const ctx = getAudioContext();
  const mix = new GainNode(ctx, {
    channelCount: 1, channelCountMode: 'explicit', channelInterpretation: 'speakers',
  });
  const samples = new Float32Array(Math.ceil(ctx.sampleRate * seconds));
  const inputs = new Map();
  let position = 0, filled = 0, node = null, stopped = false;

  loadRecordingWorklet(ctx).then(() => {
    if (stopped) return;
    node = new AudioWorkletNode(ctx, 'rec-tap', { outputChannelCount: [1] });
    node.port.onmessage = ({ data }) => {
      for (const sample of data) {
        samples[position] = sample;
        position = (position + 1) % samples.length;
      }
      filled = Math.min(samples.length, filled + data.length);
    };
    // The worklet copies input to the buffer and leaves its output silent.
    mix.connect(node).connect(ctx.destination);
  }).catch((error) => console.warn('record mix init failed:', error));

  return {
    addTrack(track) {
      if (!track || stopped || inputs.has(track)) return;
      try {
        const source = ctx.createMediaStreamSource(new MediaStream([track]));
        source.connect(mix);
        inputs.set(track, source);
      } catch (error) {
        console.warn('record mix track failed:', error);
      }
    },
    removeTrack(track) {
      inputs.get(track)?.disconnect();
      inputs.delete(track);
    },
    wav() {
      if (!filled) return null;
      const buffer = new ArrayBuffer(44 + filled * 2);
      const view = new DataView(buffer);
      const text = (offset, value) => {
        for (let i = 0; i < value.length; i++) view.setUint8(offset + i, value.charCodeAt(i));
      };
      text(0, 'RIFF');
      view.setUint32(4, 36 + filled * 2, true);
      text(8, 'WAVE');
      text(12, 'fmt ');
      view.setUint32(16, 16, true);
      view.setUint16(20, 1, true); // PCM
      view.setUint16(22, 1, true); // mono
      view.setUint32(24, ctx.sampleRate, true);
      view.setUint32(28, ctx.sampleRate * 2, true);
      view.setUint16(32, 2, true);
      view.setUint16(34, 16, true);
      text(36, 'data');
      view.setUint32(40, filled * 2, true);
      const start = (position - filled + samples.length) % samples.length;
      for (let i = 0; i < filled; i++) {
        const sample = Math.max(-1, Math.min(1, samples[(start + i) % samples.length]));
        view.setInt16(44 + i * 2, sample < 0 ? sample * 0x8000 : sample * 0x7fff, true);
      }
      return new Blob([buffer], { type: 'audio/wav' });
    },
    stop() {
      stopped = true;
      for (const source of inputs.values()) source.disconnect();
      inputs.clear();
      if (node) {
        node.port.onmessage = null;
        node.disconnect();
      }
      mix.disconnect();
    },
  };
}
