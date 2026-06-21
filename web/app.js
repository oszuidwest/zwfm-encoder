/**
 * Alpine controller for the embedded web UI.
 * REST handles configuration changes; WebSocket handles live levels/status.
 */

const DB_MINIMUM = -60;
const DB_RANGE = 60;
const MS_PER_SECOND = 1000;
const MS_PER_MINUTE = 60 * MS_PER_SECOND;
const MS_PER_DAY = 24 * 60 * MS_PER_MINUTE;
const CLIP_TIMEOUT_MS = 1500;
const WS_RECONNECT_MS = 1000;
const TEST_FEEDBACK_MS = 2000;
const TOAST_DURATION_SUCCESS = 3000;
const TOAST_DURATION_ERROR = 5000;
const BANNER_AUTO_HIDE_MS = 10000;
const EVENT_PAGE_SIZE = 50;
const MAX_TOASTS = 3;
const CONNECT_CONFIRM_MS = readCSSDuration('--duration-attention', 400);

const STORAGE_KEYS = {
    SETTINGS_CARDS: 'settingsCardsOpen',
    VU_MODE: 'vuMode',
};

const TIME_FORMATTER = new Intl.DateTimeFormat('nl-NL', {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
});

// IEC 60268-10 Type I: 20 dB fallback in 1.7 seconds.
const PPM_DECAY_DB_PER_SEC = 20 / 1.7;
const PPM_FRAME_INTERVAL_MS = 100;
const PPM_DECAY_PER_FRAME = PPM_DECAY_DB_PER_SEC * (PPM_FRAME_INTERVAL_MS / MS_PER_SECOND);

function readCSSDuration(tokenName, fallbackMs) {
    const value = getComputedStyle(document.documentElement).getPropertyValue(tokenName).trim();
    const number = Number.parseFloat(value);
    if (!Number.isFinite(number)) return fallbackMs;
    if (value.endsWith('ms')) return number;
    if (value.endsWith('s')) return number * MS_PER_SECOND;
    return fallbackMs;
}

const API = {
    CONFIG: '/api/config',
    DEVICES: '/api/devices',
    SETTINGS: '/api/settings',
    STREAMS: '/api/streams',
    RECORDERS: '/api/recorders',
    RECORDERS_TEST_S3: '/api/recorders/test-s3',
    NOTIFICATIONS_TEST: '/api/notifications/test',
    RECORDING_REGENERATE_KEY: '/api/recording/regenerate-key',
};

const EVENT_KEYS = window.eventKeyBuilders;

const EVENT_LABELS = {
    stream_started: 'Started',
    stream_stable: 'Connected',
    stream_error: 'Error',
    stream_retry: 'Retry',
    stream_stopped: 'Stopped',
    silence_start: 'Silence',
    silence_end: 'Recovered',
    audio_dump_ready: 'Audio Dump',
    channel_imbalance_start: 'Imbalance',
    channel_imbalance_end: 'Balanced',
    recorder_started: 'Started',
    recorder_stopped: 'Stopped',
    recorder_error: 'Error',
    recorder_file: 'New File',
    upload_queued: 'Upload Queued',
    upload_completed: 'Uploaded',
    upload_failed: 'Upload Failed',
    upload_retry: 'Retry',
    upload_abandoned: 'Abandoned',
    cleanup_completed: 'Cleanup',
};

const EVENT_CATEGORY_LABELS = {
    stream: 'Streams',
    audio: 'Audio',
    recorder: 'Recording',
    unknown: 'System',
};

// Status sections the event dashboard groups into, used for the filter pills.
// The key matches the keys produced by eventGroups(); label/dot drive the pill rendering.
const EVENT_GROUP_FILTERS = [
    { key: 'attention', label: 'Needs attention' },
    { key: 'resolved', label: 'Resolved' },
    { key: 'activity', label: 'Activity' },
    { key: 'routine', label: 'Routine' },
];
const EVENT_GROUP_FILTER_KEYS = EVENT_GROUP_FILTERS.map(filter => filter.key);

const RECORDER_STORAGE_LABELS = {
    both: 'Local + S3',
    s3: 'S3',
    local: 'Local',
};

/**
 * Performs an API request with an optional JSON body. Returns the parsed
 * response body, or null when there is none (e.g. 204 No Content). Throws
 * an Error with the server-supplied message on HTTP errors; validation
 * error lists are exposed on err.errors.
 */
const requestJSON = async (url, { method = 'GET', body } = {}) => {
    const options = { method };
    if (body !== undefined) {
        options.headers = { 'Content-Type': 'application/json' };
        options.body = JSON.stringify(body);
    }
    const response = await fetch(url, options);
    const result = await response.json().catch(() => null);
    if (!response.ok) {
        const error = new Error(result?.error || `HTTP ${response.status}`);
        error.errors = result?.errors;
        throw error;
    }
    return result;
};

const readStorage = (key, fallback = '') => {
    try {
        return localStorage.getItem(key) ?? fallback;
    } catch {
        return fallback;
    }
};

const readStorageObject = (key, fallback) => {
    try {
        const value = localStorage.getItem(key);
        if (!value) return fallback;
        const parsed = JSON.parse(value);
        return parsed && typeof parsed === 'object' ? { ...fallback, ...parsed } : fallback;
    } catch {
        return fallback;
    }
};

const writeStorage = (key, value) => {
    try {
        localStorage.setItem(key, value);
    } catch {
        // Storage is a non-critical UI preference.
    }
};

const copyTextFallback = (text) => {
    if (typeof document.execCommand !== 'function') {
        throw new Error('Clipboard copy is not supported');
    }

    const activeElement = document.activeElement;
    const textarea = document.createElement('textarea');
    textarea.value = text;
    textarea.setAttribute('readonly', '');
    textarea.style.position = 'fixed';
    textarea.style.top = '0';
    textarea.style.left = '0';
    textarea.style.width = '1px';
    textarea.style.height = '1px';
    textarea.style.opacity = '0';
    textarea.style.pointerEvents = 'none';

    document.body.appendChild(textarea);
    textarea.focus();
    textarea.select();
    textarea.setSelectionRange(0, textarea.value.length);

    try {
        if (!document.execCommand('copy')) {
            throw new Error('Clipboard copy failed');
        }
    } finally {
        textarea.remove();
        activeElement?.focus?.({ preventScroll: true });
    }
};

const copyTextToClipboard = async (text) => {
    if (window.isSecureContext && navigator.clipboard?.writeText) {
        try {
            await navigator.clipboard.writeText(text);
            return;
        } catch {
            // Fall through to document.execCommand for browsers that deny clipboard permissions.
        }
    }
    copyTextFallback(text);
};

/** Formats milliseconds to human-readable smart units (ms/s/m). */
const formatSmartDuration = (ms) => {
    if (ms < MS_PER_SECOND) return `${Math.round(ms)}ms`;
    if (ms < MS_PER_MINUTE) {
        const sec = ms / MS_PER_SECOND;
        return sec < 10 ? `${sec.toFixed(1)}s` : `${Math.round(sec)}s`;
    }
    const mins = Math.floor(ms / MS_PER_MINUTE);
    const secs = Math.round((ms % MS_PER_MINUTE) / MS_PER_SECOND);
    return secs > 0 ? `${mins}m ${secs}s` : `${mins}m`;
};

const msToSeconds = (ms) => ms / MS_PER_SECOND;
const secondsToMs = (sec) => Math.round(sec * MS_PER_SECOND);

const meterLevel = (db) => Number.isFinite(db) ? db : DB_MINIMUM;

/** Converts dB (-60 to 0) to percentage (0-100) for VU meter display. */
window.dbToPercent = (db) => Math.max(0, Math.min(100, (meterLevel(db) - DB_MINIMUM) / DB_RANGE * 100));

const DEFAULT_STREAM = {
    mode: 'caller',
    host: '',
    port: 8080,
    stream_id: '',
    password: '',
    has_password: false,
    clear_password: false,
    codec: 'pcm',
    bitrate: 0,
    max_retries: 99
};

const DEFAULT_LOCAL_RECORDING_PATH = '/var/lib/encoder/recordings';

const DEFAULT_RECORDER = {
    name: '',
    enabled: true,
    codec: 'mp3',
    bitrate: 0,
    recording_mode: 'hourly',
    storage_mode: 'local',
    local_path: '',
    s3_endpoint: '',
    s3_bucket: '',
    s3_access_key_id: '',
    s3_secret_access_key: '',
    has_s3_secret: false,
    clear_s3_secret: false,
    retention_days: 90
};

const storageNeedsLocal = (mode) => mode === 'local' || mode === 'both';
const storageNeedsS3 = (mode) => mode === 's3' || mode === 'both';
const streamMode = (stream) => stream?.mode || 'caller';
const recorderHasS3Secret = (form) => Boolean(
    form.s3_secret_access_key || (form.has_s3_secret && !form.clear_s3_secret)
);

/**
 * Stages removal of a saved secret: snapshots the current input so undo can
 * restore it, blanks the field, and sets the clear flag. The template
 * disables the input while the flag is set so a typed value cannot conflict
 * with the flag and 400 at the backend. Returns false when already cleared
 * or the user cancels the confirm dialog.
 */
const clearSecretField = (form, field, flag, message) => {
    if (form[flag] || !confirm(message)) return false;
    form[`_preClear_${field}`] = form[field];
    form[field] = '';
    form[flag] = true;
    return true;
};

/** Cancels a pending secret clear and restores the prior input. */
const undoClearSecretField = (form, field, flag) => {
    if (!form[flag]) return false;
    form[field] = form[`_preClear_${field}`] ?? '';
    form[flag] = false;
    form[`_preClear_${field}`] = null;
    return true;
};

// Bitrate options per codec (kbit/s). Exposed on window for Alpine.js template access.
window.CODEC_BITRATES = {
    mp3:  [64, 96, 128, 192, 256, 320],
    opus: [64, 96, 128, 192, 256],
};

// Default bitrate per codec (used when bitrate=0).
window.CODEC_DEFAULT_BITRATE = { mp3: 320, opus: 128 };

const DEFAULT_LEVELS = {
    left: -60,
    right: -60,
    peak_left: -60,
    peak_right: -60,
    silence_level: null,
    channel_imbalance_level: null,
    balance_db: 0,
    imbalance_db: 0,
    // PPM display levels (with ballistics applied)
    display_left: -60,
    display_right: -60,
    // Frontend peak hold (tracks highest display level)
    hold_left: -60,
    hold_right: -60,
    hold_left_time: 0,
    hold_right_time: 0
};

/** Formats codec and bitrate for display (e.g. "MP3 128k", "PCM"). */
window.formatCodecBitrate = (codec, bitrate) => {
    const normalized = codec || 'pcm';
    const label = normalized.toUpperCase();
    if (normalized === 'pcm') return label;
    const br = bitrate || window.CODEC_DEFAULT_BITRATE[normalized] || 0;
    return `${label} ${br}k`;
};

const deepClone = (obj) => typeof structuredClone === 'function'
    ? structuredClone(obj)
    : JSON.parse(JSON.stringify(obj));

const normalizeEventSeverity = (severity) => (
    ['error', 'warning', 'success', 'info'].includes(severity) ? severity : 'info'
);

const normalizeEventCategory = (category) => (
    ['stream', 'audio', 'recorder'].includes(category) ? category : 'unknown'
);

const normalizeEventReason = (reason) => (
    ['problem', 'recovery', 'lifecycle', 'routine'].includes(reason) ? reason : 'unknown'
);

const emptyEventGroups = () => ({
    attention: [],
    resolved: [],
    activity: [],
    routine: [],
    routineCount: 0,
});

const normalizeEventGroups = (groups = {}) => ({
    attention: Array.isArray(groups.attention) ? groups.attention : [],
    resolved: Array.isArray(groups.resolved) ? groups.resolved : [],
    activity: Array.isArray(groups.activity) ? groups.activity : [],
    routine: Array.isArray(groups.routine) ? groups.routine : [],
    routineCount: Number.isFinite(groups.routineCount) ? groups.routineCount : 0,
});

document.addEventListener('alpine:init', () => {
    Alpine.data('encoderApp', () => ({
        view: 'dashboard',
        settingsTab: 'audio',

        cardsOpen: readStorageObject(STORAGE_KEYS.SETTINGS_CARDS, {
            webhook: false, email: false, zabbix: false,
            silence: false, imbalance: false, metering: false, dumps: false, recording: false
        }),

        vuChannels: [
            { label: 'L', level: 'left', display: 'display_left', hold: 'hold_left' },
            { label: 'R', level: 'right', display: 'display_right', hold: 'hold_right' }
        ],

        settingsTabs: [
            { id: 'audio', label: 'Audio', icon: 'audio' },
            { id: 'notifications', label: 'Notifications', icon: 'email' },
            { id: 'events', label: 'Events', icon: 'list' },
            { id: 'about', label: 'About', icon: 'info' }
        ],

        streamForm: { ...DEFAULT_STREAM, id: '', enabled: true },
        streamFormDirty: false,

        encoder: {
            state: 'connecting',
            uptime: '',
            sourceRetryCount: 0,
            sourceMaxRetries: 10,
            lastError: ''
        },

        streams: [],
        streamStatuses: {},
        previousStreamStatuses: {},
        deletingStreams: {},
        connectingAnimations: {},

        recorders: [],
        recorderStatuses: {},
        deletingRecorders: {},
        recorderForm: { ...DEFAULT_RECORDER, id: '' },
        recorderFormDirty: false,

        // Event history includes stream, audio, and recorder events.
        events: [],
        eventHistoryGroups: emptyEventGroups(),
        eventFilter: '', // '' = all sections, or a group key: attention | resolved | activity | routine
        eventGroupFilters: EVENT_GROUP_FILTERS,
        eventsError: '',
        eventsLoading: false,
        eventsHasMore: false,
        eventsWindowSize: EVENT_PAGE_SIZE,
        eventOpenRows: {},

        devices: [],
        levels: { ...DEFAULT_LEVELS },
        liveAudioIssues: { silence: false, imbalance: false },
        vuMode: readStorage(STORAGE_KEYS.VU_MODE, 'peak'),
        clipActive: false,
        clipTimeout: null,

        // Configuration from REST API (fetched once, updated on config_changed)
        config: {
            audio_input: '',
            devices: [],
            platform: '',
            silence_threshold: -40,
            silence_duration_ms: 15000,
            silence_recovery_ms: 5000,
            peak_hold_ms: 3000,
            channel_imbalance_threshold: 12,
            channel_imbalance_duration_ms: 15000,
            channel_imbalance_recovery_ms: 5000,
            silence_dump: { enabled: true, retention_days: 7 },
            recording_max_duration_minutes: 240,
            webhook_has_url: false,
            zabbix_server: '',
            zabbix_port: 10051,
            zabbix_host: '',
            zabbix_silence_key: '',
            zabbix_upload_key: '',
            graph_tenant_id: '',
            graph_client_id: '',
            graph_from_address: '',
            graph_recipients: '',
            graph_has_secret: false,
            recording_has_api_key: false,
            srt_available: false,
            srt_error: '',
            streams: [],
            recorders: []
        },
        configLoaded: false,

        // Form state for settings (copied from config when entering settings view)
        settingsForm: {
            audioInput: '',
            silenceThreshold: -40,
            silenceDuration: 15,
            silenceRecovery: 5,
            peakHoldMs: 3000,
            channelImbalanceThreshold: 12,
            channelImbalanceDuration: 15,
            channelImbalanceRecovery: 5,
            silenceDump: { enabled: true, retentionDays: 7 },
            recordingMaxDurationMinutes: 240,
            silenceWebhook: '',
            clearWebhook: false,
            webhookEvents: { silence_start: true, silence_end: true, audio_dump: true },
            zabbix: { server: '', port: 10051, host: '', silenceKey: '', uploadKey: '' },
            zabbixEvents: { silence_start: true, silence_end: true },
            graph: { tenantId: '', clientId: '', clientSecret: '', clearSecret: false, fromAddress: '', recipients: '' },
            emailEvents: { silence_start: true, silence_end: true, audio_dump: true },
            recordingApiKey: '',
            platform: ''
        },
        settingsDirty: false,
        saving: false,

        graphSecretExpiry: { expires_soon: false, days_left: 0 },

        version: { current: '', latest: '', update_available: false, commit: '', build_time: '' },

        ffmpegAvailable: true, // Assume available until we get status
        recordingAvailable: true, // Assume available until we get status

        // Notification test state (unified object for all test types)
        testStates: {
            webhook: { pending: false, text: 'Test' },
            email: { pending: false, text: 'Test' },
            zabbix: { pending: false, text: 'Test' },
            recorderS3: { pending: false, text: 'Test Connection' }
        },
        _testResetTimeouts: {},

        // API key copy feedback
        apiKeyCopied: false,

        banner: {
            visible: false,
            message: '',
            type: 'info' // info, warning, danger
        },

        toasts: [],
        _toastIdCounter: 0,

        ws: null,
        _bannerTimeout: null,

        // Computed properties
        /**
         * Checks if audio source has issues (no device or capture error).
         * @returns {boolean} True if source needs attention
         */
        get hasSourceIssue() {
            return (this.encoder.sourceRetryCount > 0 && this.encoder.state !== 'stopped') ||
                   (this.encoder.lastError && this.encoder.state !== 'running');
        },

        get encoderRunning() {
            return this.encoder.state === 'running';
        },

        get isEditMode() {
            return this.streamForm.id !== '';
        },

        get isRecorderEditMode() {
            return this.recorderForm.id !== '';
        },

        /**
         * Validates recorder form and returns true if form can be saved.
         * Checks required fields based on storage mode.
         * @returns {boolean} True if form is valid and can be submitted
         */
        get canSaveRecorder() {
            if (!this.recorderForm.name?.trim()) return false;
            if (this.isRecorderEditMode && !this.recorderFormDirty) return false;

            const needsLocal = storageNeedsLocal(this.recorderForm.storage_mode);
            const needsS3 = storageNeedsS3(this.recorderForm.storage_mode);

            if (needsLocal && !this.recorderForm.local_path?.trim()) return false;
            if (needsS3) {
                if (!this.recorderForm.s3_bucket?.trim()) return false;
                if (!this.recorderForm.s3_access_key_id?.trim()) return false;
                if (!recorderHasS3Secret(this.recorderForm)) return false;
            }
            return true;
        },

        get canTestRecorderS3() {
            return Boolean(
                this.recorderForm.s3_bucket?.trim() &&
                this.recorderForm.s3_access_key_id?.trim() &&
                recorderHasS3Secret(this.recorderForm)
            );
        },

        async init() {
            await this.loadConfig();
            this.connectWebSocket();
            document.addEventListener('keydown', (e) => this.handleGlobalKeydown(e));
        },

        /**
         * Fetches configuration from REST API.
         * Called on init and when config_changed event is received.
         */
        async loadConfig() {
            try {
                const data = await requestJSON(API.CONFIG);
                this.config = data;
                this.configLoaded = true;

                // Update derived state from config
                this.streams = data.streams || [];
                this.recorders = data.recorders || [];
                this.devices = data.devices || [];
            } catch (err) {
                console.error('Failed to load config:', err);
                this.showBanner('Failed to load configuration', 'danger', true);
            }
        },

        /**
         * Refreshes the devices list from the server.
         * Called when the audio input dropdown is focused.
         */
        async refreshDevices() {
            try {
                const data = await requestJSON(API.DEVICES);
                this.devices = data.devices || [];
            } catch {
                // Silently fail - devices will show stale data
            }
        },

        /** Global keyboard: Escape closes views, Enter saves, arrows navigate tabs. */
        handleGlobalKeydown(event) {
            // Don't handle if user is typing in an input field
            const isInput = ['INPUT', 'TEXTAREA', 'SELECT'].includes(event.target.tagName);

            // Escape: close secondary views.
            if (event.key === 'Escape') {
                if (this.view === 'settings') {
                    this.cancelSettings();
                    event.preventDefault();
                } else if (this.view === 'stream-form' || this.view === 'recorder-form') {
                    this.showDashboard();
                    event.preventDefault();
                }
                return;
            }

            // Enter: Save settings (works from input fields, but not textarea/select)
            if (event.key === 'Enter' && this.view === 'settings' && this.settingsDirty) {
                const isTextareaOrSelect = ['TEXTAREA', 'SELECT'].includes(event.target.tagName);
                if (!isTextareaOrSelect) {
                    this.saveSettings();
                    event.preventDefault();
                    return;
                }
            }

            // Arrow keys: Navigate tabs in settings view
            if (this.view === 'settings' && !isInput) {
                if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
                    this.navigateTab(event.key === 'ArrowRight' ? 1 : -1);
                    event.preventDefault();
                } else if (event.key === 'Home') {
                    this.showTab(this.settingsTabs[0].id);
                    event.preventDefault();
                } else if (event.key === 'End') {
                    this.showTab(this.settingsTabs[this.settingsTabs.length - 1].id);
                    event.preventDefault();
                }
            }
        },

        /**
         * Navigates to adjacent tab in settings and focuses the tab button.
         * @param {number} direction - 1 for next, -1 for previous
         */
        navigateTab(direction) {
            const currentIndex = this.settingsTabs.findIndex(t => t.id === this.settingsTab);
            const newIndex = (currentIndex + direction + this.settingsTabs.length) % this.settingsTabs.length;
            const newTabId = this.settingsTabs[newIndex].id;
            this.showTab(newTabId);
            // Focus the new tab button for continued keyboard navigation
            this.$nextTick(() => {
                document.getElementById(`tab-${newTabId}`)?.focus();
            });
        },

        // Establishes WebSocket connection with auto-reconnect
        connectWebSocket() {
            const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
            this.ws = new WebSocket(`${protocol}//${location.host}/ws`);

            this.ws.onmessage = (e) => {
                let msg;
                try {
                    msg = JSON.parse(e.data);
                } catch {
                    console.error('Invalid WebSocket message:', e.data);
                    return;
                }
                if (msg.type === 'levels') {
                    this.handleLevels(msg.levels);
                } else if (msg.type === 'status') {
                    this.handleStatus(msg);
                } else if (msg.type === 'config_changed') {
                    // Config was changed (by this or another client), refetch
                    // Skip if we're currently editing (form open)
                    if (this.view !== 'settings' && this.view !== 'stream-form' && this.view !== 'recorder-form') {
                        this.loadConfig();
                    }
                }
            };

            this.ws.onopen = () => {
                // Clear stale toasts from before disconnect
                this.clearToasts();
            };

            this.ws.onclose = () => {
                this.encoder.state = 'connecting';
                this.resetVuMeter();
                setTimeout(() => this.connectWebSocket(), WS_RECONNECT_MS);
            };

            // Note: Don't call close() here - it triggers onclose which causes reconnect.
            // The socket will close naturally on error, triggering onclose once.
            this.ws.onerror = () => {};
        },

        /**
         * Processes incoming audio level data with PPM ballistics.
         * PPM meters have instant attack and slow decay (~20dB in 1.7s).
         * Clip indicator activates when levels exceed threshold and holds
         * for CLIP_TIMEOUT_MS before auto-clearing.
         *
         * @param {Object} levels - Audio levels (left, right, peak_left, peak_right, silence_level)
         */
        handleLevels(levels) {
            levels = { ...DEFAULT_LEVELS, ...(levels || {}) };
            const prevSilenceState = this.getSilenceState();

            // Apply PPM ballistics to peak values: instant attack, slow decay
            // This creates the characteristic PPM "slow fallback" behavior
            const applyPPM = (newPeak, displayLevel) => {
                if (newPeak >= displayLevel) {
                    return newPeak; // Instant attack - jump up to new peak
                }
                // Slow decay: reduce by PPM_DECAY_PER_FRAME, but not below new peak
                return Math.max(newPeak, displayLevel - PPM_DECAY_PER_FRAME);
            };

            const now = Date.now();
            // Must read from config, not a hardcoded constant - the user sets this via Settings.
            const peakHoldMs = this.config.peak_hold_ms ?? 3000;

            levels.display_left = applyPPM(levels.peak_left, this.levels.display_left ?? -60);
            levels.display_right = applyPPM(levels.peak_right, this.levels.display_right ?? -60);

            // Frontend peak hold: track highest display level, hold for configured duration
            // After hold expires, smoothly decay (faster than PPM bar decay)
            const HOLD_DECAY_PER_FRAME = PPM_DECAY_PER_FRAME * 2; // 2x faster decay for peak marker

            const updateHold = (display, prevHold, prevTime, key) => {
                if (display >= prevHold) {
                    // New peak - update hold and reset timer
                    levels[`hold_${key}`] = display;
                    levels[`hold_${key}_time`] = now;
                } else if (now - prevTime < peakHoldMs) {
                    // Within hold time - keep previous hold
                    levels[`hold_${key}`] = prevHold;
                    levels[`hold_${key}_time`] = prevTime;
                } else {
                    // Hold expired - smooth decay towards display level
                    const decayed = prevHold - HOLD_DECAY_PER_FRAME;
                    levels[`hold_${key}`] = Math.max(display, decayed);
                    levels[`hold_${key}_time`] = prevTime; // Keep original time
                }
            };

            updateHold(levels.display_left, this.levels.hold_left ?? -60, this.levels.hold_left_time ?? 0, 'left');
            updateHold(levels.display_right, this.levels.hold_right ?? -60, this.levels.hold_right_time ?? 0, 'right');

            this.levels = levels;
            const liveAudioIssues = {
                silence: levels.silence_level === 'active',
                imbalance: levels.channel_imbalance_level === 'active',
            };
            if (
                liveAudioIssues.silence !== this.liveAudioIssues.silence ||
                liveAudioIssues.imbalance !== this.liveAudioIssues.imbalance
            ) {
                this.liveAudioIssues = liveAudioIssues;
            }
            const newSilenceState = this.getSilenceState();

            if (newSilenceState !== prevSilenceState) {
                this.handleSilenceTransition(prevSilenceState, newSilenceState);
            }

            // Update banner message with current duration if silence banner is showing
            if (this.banner.visible && this.banner.type !== 'info' && levels.silence_duration_ms) {
                const duration = formatSmartDuration(levels.silence_duration_ms);
                if (newSilenceState === 'critical') {
                    this.banner.message = `Critical silence: ${duration}`;
                } else if (newSilenceState === 'warning') {
                    this.banner.message = `Silence detected: ${duration}`;
                }
            }

            const totalClips = (levels.clip_left || 0) + (levels.clip_right || 0);
            if (totalClips > 0) {
                this.clipActive = true;
                clearTimeout(this.clipTimeout);
                this.clipTimeout = setTimeout(() => { this.clipActive = false; }, CLIP_TIMEOUT_MS);
            }
        },

        /**
         * Handles silence state transitions and shows appropriate banners.
         * @param {string} prev - Previous silence state
         * @param {string} next - New silence state
         */
        handleSilenceTransition(prev, next) {
            const duration = formatSmartDuration(this.levels.silence_duration_ms || 0);
            if (next === 'warning' && prev === 'active') {
                this.showBanner(`Silence detected: ${duration}`, 'warning', false);
            } else if (next === 'critical') {
                this.showBanner(`Critical silence: ${duration}`, 'danger', true);
            } else if (next === '' && prev !== '') {
                // Silence recovered
                this.hideBanner();
            }
        },

        /**
         * Returns silence state for data-state attribute.
         * Thresholds are based on configured silenceDuration:
         * - active: silenceDuration (alert triggered)
         * - warning: silenceDuration * 2
         * - critical: silenceDuration * 4
         * @returns {string} Silence state: '' | 'active' | 'warning' | 'critical'
         */
        getSilenceState() {
            if (!this.levels.silence_level) return '';
            const durationMs = this.levels.silence_duration_ms || 0;
            const thresholdMs = this.config.silence_duration_ms || 15000;
            if (durationMs >= thresholdMs * 4) return 'critical';
            if (durationMs >= thresholdMs * 2) return 'warning';
            return 'active';
        },

        /**
         * Returns CSS class for silence indicator dot.
         * @returns {string} State class for dot styling
         */
        getSilenceStateClass() {
            const state = this.getSilenceState();
            if (state === 'critical') return 'state-danger';
            if (state === 'warning' || state === 'active') return 'state-warning';
            return '';
        },

        /**
         * Returns CSS class for the channel imbalance indicator dot.
         * @returns {string} State class for dot styling
         */
        getChannelImbalanceStateClass() {
            return this.levels.channel_imbalance_level === 'active' ? 'state-warning' : '';
        },

        /**
         * Returns CSS class for clip indicator dot.
         * @returns {string} State class for dot styling
         */
        getClipStateClass() {
            return this.clipActive ? 'state-danger' : '';
        },

        /**
         * Processes encoder status updates from backend.
         * Only handles runtime data (encoder state, stream/recorder statuses).
         * Configuration is handled by loadConfig() via REST API.
         *
         * @param {Object} msg - Status message with encoder state and statuses
         */
        handleStatus(msg) {
            // Feature availability
            this.ffmpegAvailable = msg.ffmpeg_available ?? true;
            this.recordingAvailable = msg.recording_available ?? true;

            // Encoder state
            this.encoder.state = msg.encoder.state;
            this.encoder.uptime = msg.encoder.uptime || '';
            this.encoder.sourceRetryCount = msg.encoder.source_retry_count || 0;
            this.encoder.sourceMaxRetries = msg.encoder.source_max_retries || 10;
            this.encoder.lastError = msg.encoder.last_error || '';

            if (!this.encoderRunning) {
                this.resetVuMeter();
            }

            // Stream statuses (not config, just runtime state)
            const newStreamStatuses = msg.stream_status || {};

            // Detect status transitions to "connected" and trigger animation
            for (const id in newStreamStatuses) {
                const oldStatus = this.previousStreamStatuses[id] || {};
                const newStatus = newStreamStatuses[id] || {};

                if (!oldStatus.stable && newStatus.stable) {
                    this.connectingAnimations[id] = true;
                    setTimeout(() => {
                        delete this.connectingAnimations[id];
                    }, CONNECT_CONFIRM_MS);
                }
            }

            this.previousStreamStatuses = deepClone(newStreamStatuses);
            this.streamStatuses = newStreamStatuses;

            // Recorder statuses
            this.recorderStatuses = msg.recorder_statuses || {};

            // Graph secret expiry (runtime info, not config)
            this.graphSecretExpiry = msg.graph_secret_expiry ?? { expires_soon: false, days_left: 0 };

            // Version info
            if (msg.version) {
                const wasUpdateAvail = this.version.update_available;
                this.version = msg.version;
                if (msg.version.update_available && !wasUpdateAvail) {
                    this.showBanner(`Update available: ${msg.version.latest}`, 'info', false);
                }
            }

            // Clean up deleting state for streams that no longer exist
            for (const id in this.deletingStreams) {
                const stream = this.streams.find(s => s.id === id);
                if (!stream || stream.created_at !== this.deletingStreams[id]) {
                    delete this.deletingStreams[id];
                }
            }

            // Clean up deleting state for recorders that no longer exist
            for (const id in this.deletingRecorders) {
                const recorder = this.recorders.find(r => r.id === id);
                if (!recorder || recorder.created_at !== this.deletingRecorders[id]) {
                    delete this.deletingRecorders[id];
                }
            }
        },


        // Navigation
        showDashboard() {
            this.clearToasts();
            this.view = 'dashboard';
            this.settingsDirty = false;
            this.streamFormDirty = false;
            this.recorderFormDirty = false;
        },

        /**
         * Navigates to settings view and creates form copy from fresh config.
         * Always fetches fresh data from server before opening.
         */
        async showSettings() {
            this.clearToasts();
            // Fetch fresh config before opening settings
            await this.loadConfig();

            // Create form copy from config (convert ms to seconds for UI)
            this.settingsForm = {
                audioInput: this.config.audio_input || '',
                silenceThreshold: this.config.silence_threshold ?? -40,
                silenceDuration: msToSeconds(this.config.silence_duration_ms ?? 15000),
                silenceRecovery: msToSeconds(this.config.silence_recovery_ms ?? 5000),
                peakHoldMs: this.config.peak_hold_ms ?? 3000,
                channelImbalanceThreshold: this.config.channel_imbalance_threshold ?? 12,
                channelImbalanceDuration: msToSeconds(this.config.channel_imbalance_duration_ms ?? 15000),
                channelImbalanceRecovery: msToSeconds(this.config.channel_imbalance_recovery_ms ?? 5000),
                silenceDump: {
                    enabled: this.config.silence_dump?.enabled ?? true,
                    retentionDays: this.config.silence_dump?.retention_days ?? 7
                },
                silenceWebhook: '', // Never pre-fill; empty = keep saved unless clearWebhook is true.
                clearWebhook: false,
                webhookEvents: {
                    silence_start: this.config.webhook_events?.silence_start ?? true,
                    silence_end: this.config.webhook_events?.silence_end ?? true,
                    audio_dump: this.config.webhook_events?.audio_dump ?? true
                },
                zabbix: {
                    server: this.config.zabbix_server || '',
                    port: this.config.zabbix_port || 10051,
                    host: this.config.zabbix_host || '',
                    silenceKey: this.config.zabbix_silence_key || '',
                    uploadKey: this.config.zabbix_upload_key || ''
                },
                zabbixEvents: {
                    silence_start: this.config.zabbix_events?.silence_start ?? true,
                    silence_end: this.config.zabbix_events?.silence_end ?? true
                },
                graph: {
                    tenantId: this.config.graph_tenant_id || '',
                    clientId: this.config.graph_client_id || '',
                    clientSecret: '', // Never pre-fill; empty = keep saved unless clearSecret is true.
                    clearSecret: false,
                    fromAddress: this.config.graph_from_address || '',
                    recipients: this.config.graph_recipients || ''
                },
                emailEvents: {
                    silence_start: this.config.email_events?.silence_start ?? true,
                    silence_end: this.config.email_events?.silence_end ?? true,
                    audio_dump: this.config.email_events?.audio_dump ?? true
                },
                recordingApiKey: '', // Only populated after regenerate so hidden keys are not copyable.
                recordingMaxDurationMinutes: this.config.recording_max_duration_minutes ?? 240,
                platform: this.config.platform || ''
            };
            this.settingsDirty = false;
            this.view = 'settings';
        },

        /**
         * Marks settings as modified, enabling Save button.
         * Called on any settings input change.
         */
        markSettingsDirty() {
            this.settingsDirty = true;
        },

        clearWebhookURL() {
            if (clearSecretField(this.settingsForm, 'silenceWebhook', 'clearWebhook',
                'Remove the saved webhook URL on save?')) this.markSettingsDirty();
        },

        undoClearWebhookURL() {
            if (undoClearSecretField(this.settingsForm, 'silenceWebhook', 'clearWebhook')) this.markSettingsDirty();
        },

        clearGraphSecret() {
            if (clearSecretField(this.settingsForm.graph, 'clientSecret', 'clearSecret',
                'Remove the saved Microsoft Graph client secret on save?')) this.markSettingsDirty();
        },

        undoClearGraphSecret() {
            if (undoClearSecretField(this.settingsForm.graph, 'clientSecret', 'clearSecret')) this.markSettingsDirty();
        },

        /**
         * Discards form changes and returns to dashboard.
         */
        cancelSettings() {
            this.showDashboard();
        },

        /**
         * Updates audio input device immediately when changed in dashboard.
         * Reverts to server state on error.
         */
        async updateAudioInput() {
            const prev = this.config.audio_input;
            try {
                await requestJSON(API.SETTINGS, {
                    method: 'POST',
                    body: {
                        audio_input: this.config.audio_input,
                        silence_threshold: this.config.silence_threshold,
                        silence_duration_ms: this.config.silence_duration_ms,
                        silence_recovery_ms: this.config.silence_recovery_ms,
                        peak_hold_ms: this.config.peak_hold_ms,
                        channel_imbalance_threshold: this.config.channel_imbalance_threshold,
                        channel_imbalance_duration_ms: this.config.channel_imbalance_duration_ms,
                        channel_imbalance_recovery_ms: this.config.channel_imbalance_recovery_ms,
                        silence_dump_enabled: this.config.silence_dump.enabled,
                        silence_dump_retention_days: this.config.silence_dump.retention_days,
                        webhook_url: '', // Empty = keep saved URL server-side.
                        clear_webhook_url: false,
                        webhook_events: this.config.webhook_events,
                        zabbix_server: this.config.zabbix_server,
                        zabbix_port: this.config.zabbix_port,
                        zabbix_host: this.config.zabbix_host,
                        zabbix_silence_key: this.config.zabbix_silence_key,
                        zabbix_upload_key: this.config.zabbix_upload_key,
                        zabbix_events: this.config.zabbix_events,
                        graph_tenant_id: this.config.graph_tenant_id,
                        graph_client_id: this.config.graph_client_id,
                        graph_client_secret: '',
                        graph_from_address: this.config.graph_from_address,
                        graph_recipients: this.config.graph_recipients,
                        email_events: this.config.email_events,
                        recording_max_duration_minutes: this.config.recording_max_duration_minutes
                    }
                });
                this.showToast('Audio input updated', 'success');
            } catch (err) {
                this.config.audio_input = prev;
                this.showRequestError(err, 'Failed to update audio input');
            }
        },

        /**
         * Persists all settings to backend via REST API.
         * Sends single atomic POST to /api/settings.
         */
        async saveSettings() {
            const form = this.settingsForm;
            if (!form) return;

            this.saving = true;

            const payload = {
                audio_input: form.audioInput,
                silence_threshold: form.silenceThreshold,
                silence_duration_ms: secondsToMs(form.silenceDuration),
                silence_recovery_ms: secondsToMs(form.silenceRecovery),
                peak_hold_ms: form.peakHoldMs,
                channel_imbalance_threshold: form.channelImbalanceThreshold,
                channel_imbalance_duration_ms: secondsToMs(form.channelImbalanceDuration),
                channel_imbalance_recovery_ms: secondsToMs(form.channelImbalanceRecovery),
                silence_dump_enabled: form.silenceDump.enabled,
                silence_dump_retention_days: form.silenceDump.retentionDays,
                webhook_url: form.silenceWebhook,
                clear_webhook_url: form.clearWebhook,
                webhook_events: form.webhookEvents,
                zabbix_server: form.zabbix.server,
                zabbix_port: form.zabbix.port,
                zabbix_host: form.zabbix.host,
                zabbix_silence_key: form.zabbix.silenceKey,
                zabbix_upload_key: form.zabbix.uploadKey,
                zabbix_events: form.zabbixEvents,
                graph_tenant_id: form.graph.tenantId,
                graph_client_id: form.graph.clientId,
                graph_client_secret: form.graph.clientSecret,
                clear_graph_client_secret: form.graph.clearSecret,
                graph_from_address: form.graph.fromAddress,
                graph_recipients: form.graph.recipients,
                email_events: form.emailEvents,
                recording_max_duration_minutes: form.recordingMaxDurationMinutes
            };

            try {
                await requestJSON(API.SETTINGS, { method: 'POST', body: payload });

                // Reload config in background to sync state
                this.loadConfig();
                this.showDashboard();
                this.showToast('Settings saved', 'success');
            } catch (err) {
                this.showRequestError(err, 'Failed to save settings');
            } finally {
                this.saving = false;
            }
        },

        showStreamForm(id = null) {
            this.clearToasts();
            if (id) {
                const stream = this.streams.find(s => s.id === id);
                if (!stream) return;
                this.streamForm = {
                    id: stream.id,
                    mode: streamMode(stream),
                    host: stream.host,
                    port: stream.port,
                    stream_id: stream.stream_id || '',
                    password: '',
                    has_password: stream.has_password || false,
                    clear_password: false,
                    codec: stream.codec || 'pcm',
                    bitrate: stream.bitrate || 0,
                    max_retries: stream.max_retries || 99,
                    enabled: stream.enabled !== false
                };
            } else {
                this.streamForm = { ...DEFAULT_STREAM, id: '', enabled: true };
            }
            this.streamFormDirty = false;
            this.view = 'stream-form';
        },

        isListenerStream(stream) {
            return streamMode(stream) === 'listener';
        },

        setStreamMode(mode) {
            const previousMode = streamMode(this.streamForm);
            if (previousMode === mode) return;
            this.streamForm.mode = mode;
            if (mode === 'listener') {
                this.streamForm.host = '';
                this.streamForm.stream_id = '';
                if (!this.isEditMode || previousMode !== 'listener') {
                    this.streamForm.codec = 'mp3';
                    this.streamForm.bitrate = 0;
                }
                if (!this.isEditMode) this.streamForm.port = 9000;
            }
            this.markStreamFormDirty();
        },

        canSaveStreamForm() {
            if (this.isEditMode && !this.streamFormDirty) return false;
            if (!this.streamForm.port || this.streamForm.port < 1 || this.streamForm.port > 65535) return false;
            if (this.streamForm.mode === 'listener') return true;
            return Boolean(this.streamForm.host?.trim());
        },

        srtUnavailableMessage() {
            return this.config.srt_error || 'SRT unavailable in this FFmpeg build.';
        },

        streamEndpoint(stream) {
            const host = this.isListenerStream(stream) && !stream.host ? '0.0.0.0' : stream.host;
            return `${host}:${stream.port}`;
        },

        streamClientHost(stream = this.streamForm) {
            const configuredHost = (stream.host || '').trim();
            const wildcard = !configuredHost || configuredHost === '0.0.0.0' || configuredHost === '::' || configuredHost === '[::]';
            const host = wildcard ? location.hostname : configuredHost;
            return host.includes(':') && !host.startsWith('[') ? `[${host}]` : host;
        },

        streamClientURL(stream = this.streamForm) {
            return `srt://${this.streamClientHost(stream)}:${stream.port || ''}?mode=caller`;
        },

        async copyStreamClientURL(stream = this.streamForm) {
            try {
                await copyTextToClipboard(this.streamClientURL(stream));
                this.showToast('Copied to clipboard', 'success');
            } catch {
                this.showToast('Failed to copy to clipboard', 'error');
            }
        },

        clearStreamPassword() {
            if (clearSecretField(this.streamForm, 'password', 'clear_password',
                'Remove the saved stream password on save?')) this.markStreamFormDirty();
        },

        undoClearStreamPassword() {
            if (undoClearSecretField(this.streamForm, 'password', 'clear_password')) this.markStreamFormDirty();
        },

        showTab(tabId) {
            this.settingsTab = tabId;
        },

        // Toggles a collapsible settings card open/closed and persists the layout.
        toggleCard(id) {
            this.cardsOpen[id] = !this.cardsOpen[id];
            writeStorage(STORAGE_KEYS.SETTINGS_CARDS, JSON.stringify(this.cardsOpen));
        },

        // Derives the header digest for a notification channel from the current
        // form + saved-config flags. Returns { summary, pill } where summary is
        // the always-shown one-liner and pill is null for the normal case or
        // { cls, label } to flag an off/attention state. Following the rest of
        // the app, only the exceptional case gets a chip (cls is a global
        // .state-* class); a configured channel stays chip-free.
        notifChannelStatus(name) {
            const f = this.settingsForm;
            const off = { cls: 'state-stopped', label: 'Off' };
            if (name === 'webhook') {
                const configured = (this.config.webhook_has_url || !!f.silenceWebhook) && !f.clearWebhook;
                if (!configured) return { summary: 'No URL set', pill: off };
                const n = Object.values(f.webhookEvents || {}).filter(Boolean).length;
                return { summary: `${n} event${n === 1 ? '' : 's'}`, pill: null };
            }
            if (name === 'email') {
                const hasSecret = this.config.graph_has_secret || !!f.graph.clientSecret;
                const configured = hasSecret && !f.graph.clearSecret &&
                    !!f.graph.tenantId && !!f.graph.clientId && !!f.graph.fromAddress && !!f.graph.recipients;
                if (!configured) return { summary: 'Not configured', pill: off };
                if (this.graphSecretExpiry.expires_soon) {
                    return { summary: `Secret expires in ${this.graphSecretExpiry.days_left} days`, pill: { cls: 'state-warning', label: 'Secret expiring' } };
                }
                const n = f.graph.recipients.split(',').map(s => s.trim()).filter(Boolean).length;
                return { summary: `${n} recipient${n === 1 ? '' : 's'}`, pill: null };
            }
            if (name === 'zabbix') {
                if (!f.zabbix.server) return { summary: 'No server set', pill: off };
                return { summary: f.zabbix.server, pill: null };
            }
            return { summary: '', pill: null };
        },

        // Derives the header digest for an Audio settings card. Returns
        // { summary, pill } (see notifChannelStatus). Always-on monitors
        // (detection, imbalance, metering) carry no chip - their summary is the
        // tuned value. Only the genuinely toggleable cards surface a chip, and
        // only when off: silence dumps disabled, or the recording API has no key.
        audioCardStatus(name) {
            const f = this.settingsForm;
            if (name === 'silence') {
                return { summary: `${f.silenceThreshold} dB / ${f.silenceDuration} s`, pill: null };
            }
            if (name === 'imbalance') {
                return { summary: `${f.channelImbalanceThreshold} dB / ${f.channelImbalanceDuration} s`, pill: null };
            }
            if (name === 'metering') {
                return { summary: `Peak hold ${(f.peakHoldMs / 1000).toFixed(1)} s`, pill: null };
            }
            if (name === 'dumps') {
                if (!f.silenceDump.enabled) return { summary: 'Not capturing', pill: { cls: 'state-stopped', label: 'Off' } };
                const d = f.silenceDump.retentionDays;
                return { summary: d > 0 ? `Kept ${d} days` : 'Kept forever', pill: null };
            }
            if (name === 'recording') {
                const hasKey = this.config.recording_has_api_key || !!f.recordingApiKey;
                const m = f.recordingMaxDurationMinutes;
                return hasKey
                    ? { summary: m > 0 ? `Max ${m} min` : 'No limit', pill: null }
                    : { summary: 'External control off', pill: { cls: 'state-stopped', label: 'No key' } };
            }
            return { summary: '', pill: null };
        },

        // Stream management (REST API)

        /**
         * Submits stream form via REST API.
         */
        async submitStreamForm() {
            if (!this.canSaveStreamForm()) return;
            const mode = streamMode(this.streamForm);

            const data = {
                mode,
                host: this.streamForm.host.trim(),
                port: this.streamForm.port,
                stream_id: mode === 'listener' ? '' : (this.streamForm.stream_id.trim() || 'studio'),
                codec: this.streamForm.codec,
                bitrate: this.streamForm.codec === 'pcm' ? 0 : (this.streamForm.bitrate || 0),
                max_retries: this.streamForm.max_retries
            };

            if (this.streamForm.password) {
                data.password = this.streamForm.password;
            }

            try {
                let result;
                if (this.isEditMode) {
                    data.enabled = this.streamForm.enabled;
                    data.clear_password = this.streamForm.clear_password || false;
                    result = await requestJSON(`${API.STREAMS}/${this.streamForm.id}`, { method: 'PUT', body: data });
                } else {
                    result = await requestJSON(API.STREAMS, { method: 'POST', body: data });
                }

                // Optimistic UI update for immediate feedback
                if (this.isEditMode) {
                    // Update status
                    if (this.streamStatuses[this.streamForm.id]) {
                        this.streamStatuses[this.streamForm.id].state = this.streamForm.enabled ? 'stopped' : 'disabled';
                    }
                    // Update stream data in local array
                    const index = this.streams.findIndex(s => s.id === this.streamForm.id);
                    if (index !== -1) {
                        Object.assign(this.streams[index], result);
                    }
                }

                // Reload config in background to sync full state
                this.loadConfig();
                const toastMsg = this.isEditMode ? 'Stream updated' : 'Stream added';
                this.showDashboard();
                this.showToast(toastMsg, 'success');
            } catch (err) {
                this.showRequestError(err, 'Failed to save stream');
            }
        },

        /**
         * Deletes stream via REST API with confirmation.
         * @param {string} id - Stream ID to delete
         * @param {boolean} [returnToDashboard=false] - Navigate to dashboard after delete
         */
        async deleteStream(id, returnToDashboard = false) {
            const stream = this.streams.find(s => s.id === id);
            if (!stream) return;

            if (!confirm(`Delete "${stream.host}:${stream.port}"? This action cannot be undone.`)) return;

            this.deletingStreams[id] = stream.created_at;

            try {
                await requestJSON(`${API.STREAMS}/${id}`, { method: 'DELETE' });

                // Success - config_changed will trigger loadConfig()
                if (returnToDashboard) this.showDashboard();
                this.showToast('Stream deleted', 'success');
            } catch (err) {
                delete this.deletingStreams[id];
                this.showToast(`Failed to delete stream: ${err.message}`, 'error');
            }
        },

        markStreamFormDirty() {
            this.streamFormDirty = true;
        },

        // Recorder management

        /**
         * Shows recorder form for adding or editing a recorder.
         * @param {string|null} id - Recorder ID for editing, null for new
         */
        showRecorderForm(id = null) {
            this.clearToasts();
            if (id) {
                const recorder = this.recorders.find(r => r.id === id);
                if (!recorder) return;
                this.recorderForm = {
                    id: recorder.id,
                    name: recorder.name,
                    enabled: recorder.enabled !== false,
                    codec: recorder.codec || 'mp3',
                    bitrate: recorder.bitrate || 0,
                    recording_mode: recorder.recording_mode || 'hourly',
                    storage_mode: recorder.storage_mode || 'local',
                    local_path: recorder.local_path || '',
                    s3_endpoint: recorder.s3_endpoint || '',
                    s3_bucket: recorder.s3_bucket || '',
                    s3_access_key_id: recorder.s3_access_key_id || '',
                    s3_secret_access_key: '',
                    has_s3_secret: recorder.has_s3_secret || false,
                    clear_s3_secret: false,
                    retention_days: recorder.retention_days || 90
                };
            } else {
                this.recorderForm = { ...DEFAULT_RECORDER, id: '' };
                if (this.config.platform === 'linux') {
                    this.recorderForm.local_path = DEFAULT_LOCAL_RECORDING_PATH;
                }
            }
            this.recorderFormDirty = false;
            this.view = 'recorder-form';
        },

        markRecorderFormDirty() {
            this.recorderFormDirty = true;
        },

        clearRecorderS3Secret() {
            if (clearSecretField(this.recorderForm, 's3_secret_access_key', 'clear_s3_secret',
                'Remove the saved S3 secret access key on save?')) this.markRecorderFormDirty();
        },

        undoClearRecorderS3Secret() {
            if (undoClearSecretField(this.recorderForm, 's3_secret_access_key', 'clear_s3_secret')) this.markRecorderFormDirty();
        },

        /**
         * Submits recorder form via REST API.
         */
        async submitRecorderForm() {
            const name = this.recorderForm.name?.trim();
            const storageMode = this.recorderForm.storage_mode;
            const localPath = this.recorderForm.local_path?.trim();
            const bucket = this.recorderForm.s3_bucket?.trim();
            const accessKey = this.recorderForm.s3_access_key_id?.trim();
            const secretKey = this.recorderForm.s3_secret_access_key;

            // Validate required fields
            if (!name) {
                this.showToast('Name is required', 'error');
                return;
            }

            // Validate based on storage mode
            const needsLocal = storageNeedsLocal(storageMode);
            const needsS3 = storageNeedsS3(storageMode);

            if (needsLocal && !localPath) {
                this.showToast('Local Path is required', 'error');
                return;
            }
            if (needsS3) {
                if (!bucket) {
                    this.showToast('S3 Bucket is required', 'error');
                    return;
                }
                if (!accessKey) {
                    this.showToast('S3 Access Key ID is required', 'error');
                    return;
                }
                if (!recorderHasS3Secret(this.recorderForm)) {
                    this.showToast('S3 Secret Access Key is required', 'error');
                    return;
                }
            }

            const data = {
                name: name,
                enabled: this.recorderForm.enabled,
                codec: this.recorderForm.codec,
                bitrate: this.recorderForm.codec === 'pcm' ? 0 : (this.recorderForm.bitrate || 0),
                recording_mode: this.recorderForm.recording_mode,
                storage_mode: storageMode,
                local_path: localPath,
                s3_endpoint: this.recorderForm.s3_endpoint.trim(),
                s3_bucket: bucket,
                s3_access_key_id: accessKey,
                retention_days: this.recorderForm.retention_days || 90
            };

            if (secretKey) {
                data.s3_secret_access_key = secretKey;
            }

            try {
                let result;
                if (this.isRecorderEditMode) {
                    data.clear_s3_secret = this.recorderForm.clear_s3_secret || false;
                    result = await requestJSON(`${API.RECORDERS}/${this.recorderForm.id}`, { method: 'PUT', body: data });
                } else {
                    result = await requestJSON(API.RECORDERS, { method: 'POST', body: data });
                }

                // Optimistic UI update for immediate feedback
                if (this.isRecorderEditMode) {
                    // Update status
                    if (this.recorderStatuses[this.recorderForm.id]) {
                        this.recorderStatuses[this.recorderForm.id].state = this.recorderForm.enabled ? 'stopped' : 'disabled';
                    }
                    // Update recorder data in local array
                    const index = this.recorders.findIndex(r => r.id === this.recorderForm.id);
                    if (index !== -1) {
                        Object.assign(this.recorders[index], result);
                    }
                }

                // Reload config in background to sync full state
                this.loadConfig();
                const toastMsg = this.isRecorderEditMode ? 'Recorder updated' : 'Recorder added';
                this.showDashboard();
                this.showToast(toastMsg, 'success');
            } catch (err) {
                this.showRequestError(err, 'Failed to save recorder');
            }
        },

        /**
         * Deletes a recorder via REST API with confirmation.
         * @param {string} id - Recorder ID to delete
         * @param {boolean} returnToDashboard - Navigate to dashboard after delete
         */
        async deleteRecorder(id, returnToDashboard = false) {
            const recorder = this.recorders.find(r => r.id === id);
            if (!recorder) return;

            if (!confirm(`Delete "${recorder.name}"? This action cannot be undone.`)) return;

            this.deletingRecorders[id] = recorder.created_at;

            try {
                await requestJSON(`${API.RECORDERS}/${id}`, { method: 'DELETE' });

                // Success - config_changed will trigger loadConfig()
                if (returnToDashboard) this.showDashboard();
                this.showToast('Recorder deleted', 'success');
            } catch (err) {
                delete this.deletingRecorders[id];
                this.showToast(`Failed to delete recorder: ${err.message}`, 'error');
            }
        },

        /**
         * Starts or stops recording via REST API.
         * @param {string} id - Recorder ID
         * @param {string} action - 'start' or 'stop'
         */
        async recorderAction(id, action) {
            try {
                await requestJSON(`${API.RECORDERS}/${id}/${action}`, { method: 'POST' });
            } catch (err) {
                this.showToast(`Failed to ${action} recorder: ${err.message}`, 'error');
            }
        },

        /**
         * Tests S3 connection via REST API.
         */
        async testRecorderS3() {
            this.startTestState('recorderS3');

            try {
                const payload = {
                    s3_endpoint: this.recorderForm.s3_endpoint,
                    s3_bucket: this.recorderForm.s3_bucket,
                    s3_access_key_id: this.recorderForm.s3_access_key_id,
                    s3_secret_access_key: this.recorderForm.s3_secret_access_key
                };
                if (this.isRecorderEditMode) {
                    payload.recorder_id = this.recorderForm.id;
                }

                await requestJSON(API.RECORDERS_TEST_S3, { method: 'POST', body: payload });
                this.finishTestState('recorderS3', 'Connected!', 'Test Connection');
            } catch (err) {
                this.finishTestState('recorderS3', 'Failed', 'Test Connection');
                this.showToast(`S3 test failed: ${err.message}`, 'error');
            }
        },

        /**
         * Computes all display data for a recorder in a single call.
         * @param {Object} recorder - Recorder object
         * @returns {Object} Display data with stateClass, statusText, duration
         */
        getRecorderDisplayData(recorder) {
            const status = this.recorderStatuses[recorder.id] || {};
            const isDeleting = this.deletingRecorders[recorder.id] === recorder.created_at;

            let stateClass = 'state-stopped';
            let statusText = 'Idle';

            if (isDeleting) {
                stateClass = 'state-warning';
                statusText = 'Deleting...';
            } else {
                switch (status.state) {
                    case 'disabled':
                        stateClass = 'state-stopped';
                        statusText = 'Disabled';
                        break;
                    case 'starting':
                        stateClass = 'state-warning';
                        statusText = 'Starting...';
                        break;
                    case 'running':
                        stateClass = 'state-success';
                        statusText = 'Recording';
                        break;
                    case 'rotating':
                        stateClass = 'state-warning';
                        statusText = 'Rotating...';
                        break;
                    case 'stopping':
                        stateClass = 'state-warning';
                        statusText = 'Finalizing...';
                        break;
                    case 'error':
                        stateClass = 'state-danger';
                        statusText = status.error || 'Error';
                        break;
                    default:
                        stateClass = 'state-stopped';
                        statusText = 'Idle';
                        break;
                }
            }

            return {
                stateClass,
                statusText
            };
        },

        /**
         * Computes all display data for a stream in a single call.
         * Use this method to avoid multiple getStreamStatus() calls per render.
         *
         * @param {Object} stream - Stream object with id and created_at
         * @returns {Object} Object with stateClass, statusText, showError, and lastError
         */
        getStreamDisplayData(stream) {
            const status = this.streamStatuses[stream.id] || {};
            const isDeleting = this.deletingStreams[stream.id] === stream.created_at;
            const isListener = this.isListenerStream(stream);

            let stateClass = 'state-stopped';
            let statusText = 'Offline';

            if (isDeleting) {
                stateClass = 'state-warning';
                statusText = 'Deleting...';
            } else {
                switch (status.state) {
                    case 'disabled':
                        stateClass = 'state-stopped';
                        statusText = 'Disabled';
                        break;
                    case 'starting':
                        stateClass = 'state-warning';
                        statusText = isListener ? 'Starting...' : 'Connecting...';
                        break;
                    case 'running':
                        if (isListener) {
                            if (status.encoder_running) {
                                stateClass = 'state-success';
                                statusText = status.uptime ? `Ready (${status.uptime})` : 'Ready';
                                if (status.listener_drops > 0) {
                                    stateClass = 'state-warning';
                                    statusText = `${statusText}, ${status.listener_drops} dropped`;
                                }
                            } else if (status.retry_count > 0) {
                                stateClass = 'state-warning';
                                statusText = 'Restarting encoder';
                            } else {
                                stateClass = 'state-warning';
                                statusText = 'Starting encoder';
                            }
                        } else if (status.stable) {
                            stateClass = 'state-success';
                            statusText = status.uptime ? `Connected (${status.uptime})` : 'Connected';
                        } else {
                            stateClass = 'state-warning';
                            statusText = 'Connecting...';
                        }
                        break;
                    case 'stopping':
                        stateClass = 'state-warning';
                        statusText = 'Stopping...';
                        break;
                    case 'error':
                        if (status.exhausted) {
                            stateClass = 'state-danger';
                            statusText = 'Failed';
                        } else if (status.retry_count > 0) {
                            stateClass = 'state-warning';
                            statusText = `Retry ${status.retry_count}/${status.max_retries}`;
                        } else {
                            stateClass = 'state-danger';
                            statusText = 'Error';
                        }
                        break;
                    default:
                        stateClass = 'state-stopped';
                        statusText = 'Offline';
                        break;
                }
            }

            // Compute error visibility
            const showError = !isDeleting && status.state === 'error' && status.error;

            return {
                stateClass,
                statusText,
                showError,
                lastError: status.error || ''
            };
        },

        toggleVuMode() {
            this.vuMode = this.vuMode === 'peak' ? 'rms' : 'peak';
            writeStorage(STORAGE_KEYS.VU_MODE, this.vuMode);
        },

        resetVuMeter() {
            this.levels = { ...DEFAULT_LEVELS };
        },

        /**
         * Loads events from the API (stream, audio, and recorder events).
         * Always requests the full newest-first window (no server-side
         * filtering) so the server can pair problem/recovery events across it;
         * the status-section filter is applied in the template via
         * sectionVisible().
         * @param {boolean} reset - If true, resets pagination and replaces events
         */
        async loadEvents(reset = true) {
            if (reset) {
                this.eventsWindowSize = EVENT_PAGE_SIZE;
                this.events = [];
                this.eventHistoryGroups = emptyEventGroups();
                this.eventOpenRows = {};
            }
            this.eventsError = '';
            this.eventsLoading = true;
            try {
                // The API groups over the returned window, so load-more expands
                // the newest-first window instead of asking for a separate page.
                const params = new URLSearchParams({
                    limit: String(this.eventsWindowSize),
                    offset: '0',
                });
                const data = await requestJSON(`/api/events?${params}`);
                const newEvents = data.events || [];
                this.events = newEvents;
                this.eventHistoryGroups = normalizeEventGroups(data.groups);
                this.eventsHasMore = data.has_more || false;
                return true;
            } catch (error) {
                console.error('Failed to load events:', error);
                this.eventsError = `Could not load events: ${error?.message || 'request failed'}`;
                return false;
            } finally {
                this.eventsLoading = false;
            }
        },

        /**
         * Loads more events (pagination).
         */
        async loadMoreEvents() {
            const previousWindowSize = this.eventsWindowSize;
            this.eventsWindowSize += EVENT_PAGE_SIZE;
            const loaded = await this.loadEvents(false);
            if (!loaded) this.eventsWindowSize = previousWindowSize;
        },

        formatEventTime(ts) {
            if (!ts) return '';
            return TIME_FORMATTER.format(new Date(ts));
        },

        formatRelativeEventTime(value) {
            const time = typeof value === 'number' ? value : new Date(value).getTime();
            if (!Number.isFinite(time)) return '';

            const diff = Math.max(0, Date.now() - time);
            if (diff < MS_PER_MINUTE) return 'just now';
            if (diff < 60 * MS_PER_MINUTE) return `${Math.floor(diff / MS_PER_MINUTE)}m ago`;
            if (diff < MS_PER_DAY) return `${Math.floor(diff / (60 * MS_PER_MINUTE))}h ago`;
            return `${Math.floor(diff / MS_PER_DAY)}d ago`;
        },

        eventTimeValue(event) {
            const time = new Date(event?.ts).getTime();
            return Number.isFinite(time) ? time : 0;
        },

        getEventSeverity(event) {
            return normalizeEventSeverity(event?.severity);
        },

        getEventCategory(event) {
            return normalizeEventCategory(event?.category);
        },

        getEventReason(event) {
            return normalizeEventReason(event?.reason);
        },

        getEventLabel(type) {
            return EVENT_LABELS[type] || type;
        },

        getEventDetail(event) {
            const details = event.details || {};
            const streamName = details.stream_name || '';

            if (event.type === 'stream_error') {
                return details.error || 'Unknown error';
            }
            if (event.type === 'stream_retry') {
                const retryNum = details.retry ? `Retry #${details.retry}` : '';
                const error = details.error || '';
                return [retryNum, error].filter(Boolean).join(' - ');
            }
            if (event.type === 'silence_start') {
                if (details.level_left_db !== undefined) {
                    return `L: ${details.level_left_db.toFixed(1)}dB  R: ${details.level_right_db.toFixed(1)}dB`;
                }
                return '';
            }
            if (event.type === 'silence_end') {
                return details.duration_ms ? `Duration: ${formatSmartDuration(details.duration_ms)}` : '';
            }
            if (event.type === 'channel_imbalance_start') {
                if (details.imbalance_db !== undefined) {
                    return `L: ${details.level_left_db.toFixed(1)}dB  R: ${details.level_right_db.toFixed(1)}dB (${details.imbalance_db.toFixed(1)}dB diff)`;
                }
                return '';
            }
            if (event.type === 'channel_imbalance_end') {
                return details.duration_ms ? `Duration: ${formatSmartDuration(details.duration_ms)}` : '';
            }
            if (event.type === 'audio_dump_ready') {
                if (details.dump_error) return `Error: ${details.dump_error}`;
                if (details.dump_filename) return details.dump_filename;
                return '';
            }
            // Recorder events
            if (event.type === 'recorder_error' || event.type === 'upload_failed') {
                return details.error || 'Unknown error';
            }
            if (event.type === 'upload_retry' || event.type === 'upload_abandoned') {
                const retryNum = details.retry ? `Retry #${details.retry}` : '';
                const error = details.error || '';
                return [retryNum, error].filter(Boolean).join(' - ') || 'Unknown error';
            }
            if (event.type === 'recorder_file' || event.type === 'upload_queued' || event.type === 'upload_completed') {
                const filename = details.filename || '';
                const codec = details.codec || '';
                return [filename, codec.toUpperCase()].filter(Boolean).join(' - ');
            }
            if (event.type === 'cleanup_completed') {
                const count = details.files_deleted || 0;
                const storage = details.storage_type || '';
                return `${count} files deleted (${storage})`;
            }
            if (event.type === 'recorder_started' || event.type === 'recorder_stopped') {
                const codec = details.codec ? details.codec.toUpperCase() : '';
                const mode = details.storage_mode || '';
                const modeLabel = RECORDER_STORAGE_LABELS[mode] || '';
                return [codec, modeLabel].filter(Boolean).join(' - ');
            }
            // For started/stable/stopped, show stream name
            return streamName;
        },

        getEventSource(event) {
            const details = event.details || {};
            switch (this.getEventCategory(event)) {
                case 'audio':
                    return 'Audio';
                case 'recorder':
                    return details.recorder_name || 'Recorder';
                case 'stream':
                    return details.stream_name || event.stream_id || 'Stream';
                default:
                    return EVENT_CATEGORY_LABELS.unknown;
            }
        },

        eventGroups() {
            const history = this.eventHistoryGroups || emptyEventGroups();
            const liveAttention = this.buildLiveAttentionItems();
            const liveDedupeKeys = new Set(
                liveAttention.flatMap(item => this.eventItemDedupeKeys(item)),
            );
            return {
                attention: [
                    ...liveAttention,
                    ...history.attention.filter(item => (
                        !this.eventItemDedupeKeys(item).some(key => liveDedupeKeys.has(key))
                    )),
                ],
                resolved: history.resolved,
                activity: history.activity,
                routine: history.routine,
                routineCount: history.routineCount,
            };
        },

        // eventGroupCount returns how many items the named section contributes.
        // Routine collapses to one row but reports its underlying event count.
        eventGroupCount(groups, key) {
            if (key === 'routine') return groups.routineCount || 0;
            return (groups[key] || []).length;
        },

        // sectionVisible reports whether a section should render given the active
        // status filter: it must be non-empty and either unfiltered or selected.
        sectionVisible(groups, key) {
            return this.eventGroupCount(groups, key) > 0 &&
                (!this.eventFilter || this.eventFilter === key);
        },

        // visibleEventGroupKeys lists the sections to render under the active filter.
        visibleEventGroupKeys(groups) {
            return EVENT_GROUP_FILTER_KEYS.filter(key => this.sectionVisible(groups, key));
        },

        eventSummaryState(groups) {
            if (this.eventsError) return 'error';
            return groups.attention.length > 0 ? 'attention' : 'clear';
        },

        eventSummaryText(groups) {
            if (this.eventsError) return 'History unavailable';
            if (groups.attention.length > 0) {
                const resolved = groups.resolved.length > 0 ? ` / ${groups.resolved.length} resolved` : '';
                return `${groups.attention.length} need attention${resolved}`;
            }
            return 'All clear';
        },

        eventSummaryMutedText(groups) {
            if (this.eventsError) return 'Could not load events';
            const parts = [];
            if (groups.attention.length === 0 && groups.resolved.length > 0) parts.push(`${groups.resolved.length} resolved`);
            if (groups.activity.length > 0) parts.push(`${groups.activity.length} activity`);
            if (groups.routineCount > 0) parts.push(`${groups.routineCount} routine`);
            if (parts.length > 0) return parts.join(', ');
            return groups.attention.length > 0 ? '' : 'No history loaded';
        },

        eventItemWhen(item, section) {
            if (item.when) return item.when;

            const time = this.eventItemReferenceTime(item, section);
            if (!time) return '';

            const relative = this.formatRelativeEventTime(time);
            if (!relative) return '';
            if (section === 'routine') return `last ${relative}`;
            if (item.statusText === 'Ongoing') return `since ${relative}`;
            return relative;
        },

        eventItemReferenceTime(item, section) {
            const events = item.events || [];
            if (section === 'resolved' && events.length > 0) {
                const lastTime = this.eventTimeValue(events[events.length - 1]);
                if (lastTime) return lastTime;
            }
            if (Number.isFinite(item.sortTs)) return item.sortTs;
            return this.eventTimeValue(events[0]);
        },

        buildLiveAttentionItems() {
            const items = [];
            for (const stream of this.streams) {
                const item = this.buildLiveStreamIssue(stream);
                if (item) items.push(item);
            }
            const silence = this.buildLiveSilenceIssue();
            const imbalance = this.buildLiveImbalanceIssue();
            if (silence) items.push(silence);
            if (imbalance) items.push(imbalance);
            for (const recorder of this.recorders) {
                const item = this.buildLiveRecorderIssue(recorder);
                if (item) items.push(item);
            }
            return items;
        },

        buildLiveStreamIssue(stream) {
            if (!stream.enabled) return null;
            const status = this.streamStatuses[stream.id];
            if (!status?.state) return null;

            const isListener = this.isListenerStream(stream);
            const problem = isListener
                ? status.exhausted || status.state === 'error' || (status.state === 'running' && !status.encoder_running)
                : !(status.state === 'running' && status.stable && !status.exhausted);
            if (!problem) return null;

            const latest = this.latestStreamProblemEvent(stream.id);
            const severity = status.exhausted || status.state === 'error' ? 'error' : 'warning';
            const tries = status.retry_count ? `${status.retry_count} ${status.retry_count === 1 ? 'try' : 'tries'}` : '';
            const title = isListener ? 'Listener encoder issue' : 'Stream disconnected';
            const detail = status.error || (latest ? this.getEventDetail(latest) : '') || this.streamIssueDetail(status, isListener);

            return {
                key: `live:stream:${stream.id}`,
                // Keep live source keys byte-identical to backend dedupe keys in internal/eventlog/groups.go.
                sourceKey: EVENT_KEYS.streamSourceKey(stream.id),
                category: 'stream',
                severity,
                title,
                source: this.streamEndpoint(stream),
                statusText: status.exhausted ? 'Failed' : 'Ongoing',
                chips: tries ? [tries] : [],
                detail,
                when: latest ? `since ${this.formatRelativeEventTime(latest.ts)}` : 'now',
                events: latest ? [latest] : [],
            };
        },

        streamIssueDetail(status, isListener) {
            if (status.state === 'running' && isListener && !status.encoder_running) return 'Encoder process is not running';
            if (status.state === 'running') return 'Waiting for stable output';
            if (status.state === 'starting') return isListener ? 'Starting listener' : 'Connecting';
            if (status.state === 'stopping') return 'Stopping';
            return status.state || 'Unavailable';
        },

        latestStreamProblemEvent(streamID) {
            return this.events.find(event => (
                this.getEventCategory(event) === 'stream' &&
                this.getEventReason(event) === 'problem' &&
                event.stream_id === streamID
            ));
        },

        buildLiveSilenceIssue() {
            if (!this.liveAudioIssues.silence) return null;
            const latest = this.events.find(event => event.type === 'silence_start');
            return {
                key: 'live:audio:silence',
                // Keep live source keys byte-identical to backend dedupe keys in internal/eventlog/groups.go.
                sourceKey: EVENT_KEYS.audioSilenceSourceKey(),
                category: 'audio',
                severity: 'warning',
                title: 'Silence on input',
                source: 'Audio',
                statusText: 'Ongoing',
                chips: [],
                detail: latest ? this.getEventDetail(latest) : 'Silence active',
                when: latest ? `since ${this.formatRelativeEventTime(latest.ts)}` : 'now',
                events: latest ? [latest] : [],
            };
        },

        buildLiveImbalanceIssue() {
            if (!this.liveAudioIssues.imbalance) return null;
            const latest = this.events.find(event => event.type === 'channel_imbalance_start');
            return {
                key: 'live:audio:imbalance',
                // Keep live source keys byte-identical to backend dedupe keys in internal/eventlog/groups.go.
                sourceKey: EVENT_KEYS.audioImbalanceSourceKey(),
                category: 'audio',
                severity: 'warning',
                title: 'Channel imbalance',
                source: 'Audio',
                statusText: 'Ongoing',
                chips: [],
                detail: latest ? this.getEventDetail(latest) : 'Channel imbalance active',
                when: latest ? `since ${this.formatRelativeEventTime(latest.ts)}` : 'now',
                events: latest ? [latest] : [],
            };
        },

        buildLiveRecorderIssue(recorder) {
            if (!recorder.enabled) return null;
            const status = this.recorderStatuses[recorder.id];
            if (!status?.state) return null;

            const pending = status.pending_uploads || 0;
            const errored = status.state === 'error';
            if (!errored && pending === 0) return null;

            const chips = [];
            if (pending > 0) chips.push(`${pending} retry upload${pending === 1 ? '' : 's'}`);
            if (recorder.recording_mode) chips.push(recorder.recording_mode);
            const sourceKey = errored
                ? this.recorderStatusSourceKey(recorder.name)
                : this.recorderUploadSourceKey(recorder.name);

            return {
                key: `live:recorder:${recorder.id}`,
                sourceKey,
                category: 'recorder',
                severity: errored ? 'error' : 'warning',
                title: errored ? 'Recorder error' : 'Upload retry pending',
                source: recorder.name || 'Recorder',
                statusText: errored ? 'Unresolved' : 'Ongoing',
                chips,
                detail: status.error || '',
                when: 'now',
                events: this.latestRecorderProblemEvents(recorder.name),
            };
        },

        recorderStatusSourceKey(recorderName) {
            // Keep live recorder keys byte-identical to backend dedupe keys in internal/eventlog/groups.go.
            return EVENT_KEYS.recorderStatusSourceKey(recorderName);
        },

        recorderUploadSourceKey(recorderName) {
            // Upload keys are intentionally recorder-wide so live retry state hides per-file history.
            return EVENT_KEYS.recorderUploadSourceKey(recorderName);
        },

        latestRecorderProblemEvents(recorderName) {
            return this.events.filter(event => {
                const details = event.details || {};
                return this.getEventCategory(event) === 'recorder' &&
                    this.getEventReason(event) === 'problem' &&
                    details.recorder_name === recorderName;
            }).slice(0, 3);
        },

        eventItemDedupeKeys(item) {
            // Keep these keys aligned with live issue keys so live state suppresses matching history.
            return [item.sourceKey, item.dedupeKey].filter(Boolean);
        },

        eventRowKey(event, index) {
            return `event:${event.type}:${event.ts}:${event.stream_id || ''}:${index}`;
        },

        eventRowStateClass(item) {
            switch (item.severity) {
                case 'error':
                    return 'state-danger';
                case 'warning':
                    return 'state-warning';
                case 'success':
                    return 'state-success';
                default:
                    return 'state-stopped';
            }
        },

        eventItemHasTrail(item) {
            return (item.events || []).length > 1;
        },

        visibleEventTrail(item) {
            const events = item.events || [];
            return item.trailLimit ? events.slice(0, item.trailLimit) : events;
        },

        eventTrailMoreCount(item) {
            const events = item.events || [];
            return item.trailLimit && events.length > item.trailLimit ? events.length - item.trailLimit : 0;
        },

        toggleEventRow(key) {
            this.eventOpenRows = { ...this.eventOpenRows, [key]: !this.eventOpenRows[key] };
        },

        isEventRowOpen(key) {
            return Boolean(this.eventOpenRows[key]);
        },

        /**
         * Triggers a notification test via REST API.
         * Uses form values (unsaved) for testing.
         *
         * @param {string} type - Test type: 'webhook', 'email', or 'zabbix'
         */
        async sendTest(type) {
            if (!this.testStates[type]) return;
            this.startTestState(type);

            // Build payload with current form values
            const payload = {};
            if (this.settingsForm) {
                if (type === 'webhook') {
                    if (this.settingsForm.silenceWebhook) {
                        payload.webhook_url = this.settingsForm.silenceWebhook;
                    }
                } else if (type === 'email') {
                    payload.graph_tenant_id = this.settingsForm.graph.tenantId;
                    payload.graph_client_id = this.settingsForm.graph.clientId;
                    payload.graph_from_address = this.settingsForm.graph.fromAddress;
                    payload.graph_recipients = this.settingsForm.graph.recipients;
                    if (this.settingsForm.graph.clientSecret) {
                        payload.graph_client_secret = this.settingsForm.graph.clientSecret;
                    }
                } else if (type === 'zabbix') {
                    payload.zabbix_server = this.settingsForm.zabbix.server;
                    payload.zabbix_port = this.settingsForm.zabbix.port;
                    payload.zabbix_host = this.settingsForm.zabbix.host;
                    payload.zabbix_silence_key = this.settingsForm.zabbix.silenceKey;
                    payload.zabbix_upload_key = this.settingsForm.zabbix.uploadKey;
                }
            }

            try {
                await requestJSON(`${API.NOTIFICATIONS_TEST}/${type}`, { method: 'POST', body: payload });
                this.finishTestState(type, 'Sent!');
            } catch (err) {
                this.finishTestState(type, 'Failed');
                const typeName = type.charAt(0).toUpperCase() + type.slice(1);
                this.showToast(`${typeName} test failed: ${err.message}`, 'error');
            }
        },


        /**
         * Regenerates the API key for recording endpoints via REST API.
         */
        async regenerateApiKey() {
            if (!confirm('Regenerate API key? Existing integrations will stop working.')) return;

            try {
                const result = await requestJSON(API.RECORDING_REGENERATE_KEY, { method: 'POST' });

                if (result?.api_key) {
                    // Update form if open - field update is the feedback
                    if (this.settingsForm) {
                        this.settingsForm.recordingApiKey = result.api_key;
                    }
                    this.config.recording_has_api_key = true;
                    this.showToast('API key regenerated', 'success');
                }
            } catch (err) {
                this.showToast(`Failed to regenerate API key: ${err.message}`, 'error');
            }
        },

        /**
         * Copies the API key to clipboard.
         */
        async copyApiKey() {
            const key = this.settingsForm?.recordingApiKey;
            if (!key) return;
            try {
                await copyTextToClipboard(key);
                this.apiKeyCopied = true;
                this.showToast('Copied to clipboard', 'success');
                setTimeout(() => { this.apiKeyCopied = false; }, TEST_FEEDBACK_MS);
            } catch {
                this.showToast('Failed to copy to clipboard', 'error');
            }
        },

        /**
         * Shows an alert banner notification.
         * Clears any existing auto-hide timeout to prevent race conditions.
         * @param {string} message - Message to display
         * @param {string} type - Banner type: 'info', 'warning', 'danger'
         * @param {boolean} persistent - If true, banner stays until dismissed
         */
        showBanner(message, type = 'info', persistent = false) {
            // Clear existing timeout to prevent premature hiding of new banner
            if (this._bannerTimeout) {
                clearTimeout(this._bannerTimeout);
                this._bannerTimeout = null;
            }
            this.banner = { visible: true, message, type };
            if (!persistent) {
                this._bannerTimeout = setTimeout(() => this.hideBanner(), BANNER_AUTO_HIDE_MS);
            }
        },

        hideBanner() {
            if (this._bannerTimeout) {
                clearTimeout(this._bannerTimeout);
                this._bannerTimeout = null;
            }
            this.banner.visible = false;
        },

        showRequestError(err, fallbackMessage) {
            if (err.errors?.length) {
                for (const msg of err.errors) {
                    this.showToast(msg, 'error');
                }
                return;
            }
            this.showToast(`${fallbackMessage}: ${err.message}`, 'error');
        },

        startTestState(type) {
            if (this._testResetTimeouts[type]) {
                clearTimeout(this._testResetTimeouts[type]);
                delete this._testResetTimeouts[type];
            }
            this.testStates[type].pending = true;
            this.testStates[type].text = 'Testing...';
        },

        finishTestState(type, text, resetText = 'Test') {
            this.testStates[type].pending = false;
            this.testStates[type].text = text;
            this._testResetTimeouts[type] = setTimeout(() => {
                if (this.testStates[type]) this.testStates[type].text = resetText;
                delete this._testResetTimeouts[type];
            }, TEST_FEEDBACK_MS);
        },

        /**
         * Shows a toast notification for transient feedback.
         * Toasts auto-dismiss based on type: success=3s, error=5s.
         * Maximum 3 toasts visible; oldest removed when exceeded.
         * @param {string} message - Message to display
         * @param {string} type - Toast type: 'success', 'error', 'info'
         */
        showToast(message, type = 'success') {
            const id = ++this._toastIdCounter;
            const duration = type === 'error' ? TOAST_DURATION_ERROR : TOAST_DURATION_SUCCESS;

            const toast = {
                id,
                message,
                type,
                timeoutId: setTimeout(() => this.removeToast(id), duration)
            };

            // Add to front (new on top due to column-reverse CSS)
            this.toasts.unshift(toast);

            // Remove oldest if exceeding max
            while (this.toasts.length > MAX_TOASTS) {
                const oldest = this.toasts.pop();
                if (oldest.timeoutId) clearTimeout(oldest.timeoutId);
            }
        },

        /**
         * Removes a specific toast by ID.
         * @param {number} id - Toast ID to remove
         */
        removeToast(id) {
            const index = this.toasts.findIndex(t => t.id === id);
            if (index !== -1) {
                const toast = this.toasts[index];
                if (toast.timeoutId) clearTimeout(toast.timeoutId);
                this.toasts.splice(index, 1);
            }
        },

        /**
         * Clears all toasts and their timeouts.
         * Called on view navigation and WebSocket reconnect.
         */
        clearToasts() {
            for (const toast of this.toasts) {
                if (toast.timeoutId) clearTimeout(toast.timeoutId);
            }
            this.toasts = [];
        }
    }));
});
