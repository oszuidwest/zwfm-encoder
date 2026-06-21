(function initEventKeyBuilders(root) {
    root.eventKeyBuilders = {
        streamSourceKey(streamID) {
            return streamID ? `stream:${streamID}` : '';
        },
        audioSilenceSourceKey() {
            return 'audio:silence';
        },
        audioImbalanceSourceKey() {
            return 'audio:imbalance';
        },
        recorderStatusSourceKey(recorderName) {
            return recorderName ? `recorder:${recorderName}:` : '';
        },
        recorderUploadSourceKey(recorderName) {
            return recorderName ? `recorder-upload:${recorderName}` : '';
        },
    };
})(globalThis);
