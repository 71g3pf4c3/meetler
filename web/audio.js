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

export function createRecordingMix(onSamples) {
  let mix = null, node = null, mute = null;
  const inputs = new Map();
  return {
    start() {
      const ctx = getAudioContext();
      if (mix) return ctx.sampleRate;
      const currentMix = new GainNode(ctx, {
        channelCount: 1, channelCountMode: 'explicit', channelInterpretation: 'speakers',
      });
      mix = currentMix;
      loadRecordingWorklet(ctx).then(() => {
        if (mix !== currentMix) return;
        node = new AudioWorkletNode(ctx, 'rec-tap', { outputChannelCount: [1] });
        node.port.onmessage = ({ data }) => {
          if (mix === currentMix) onSamples(data);
        };
        mute = new GainNode(ctx, { gain: 0 });
        currentMix.connect(node);
        node.connect(mute);
        mute.connect(ctx.destination);
      }).catch((error) => console.warn('record mix init failed:', error));
      return ctx.sampleRate;
    },
    addTrack(track) {
      if (!track || !mix || inputs.has(track)) return;
      try {
        const source = getAudioContext().createMediaStreamSource(new MediaStream([track]));
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
    stop() {
      for (const source of inputs.values()) source.disconnect();
      inputs.clear();
      if (node) {
        node.port.onmessage = null;
        node.disconnect();
      }
      mix?.disconnect();
      mute?.disconnect();
      mix = node = mute = null;
    },
  };
}
