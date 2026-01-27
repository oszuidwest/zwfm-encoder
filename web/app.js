/**
 * ZuidWest FM Encoder - Alpine.js Web Application
 *
 * Real-time audio monitoring, encoder control, and multi-stream
 * management via WebSocket and REST API connection to Go backend.
 *
 * Architecture:
 *   - Single Alpine.js component (encoderApp) manages all UI state
 *   - REST API at /api/* for configuration (settings, streams, recorders)
 *   - WebSocket at /ws for real-time data (audio levels, status)
 *   - Views: dashboard, settings, stream-form, recorder-form
 *
 * REST API (configuration):
 *   - GET  /api/config: Full configuration snapshot
 *   - POST /api/settings: Update all settings atomically
 *   - POST/GET/PUT/DELETE /api/streams/*: Stream CRUD
 *   - POST/GET/PUT/DELETE /api/recorders/*: Recorder CRUD
 *   - POST /api/notifications/test/*: Test notifications
 *   - GET  /api/notifications/log: View silence log
 *
 * WebSocket (real-time):
 *   - levels: Audio RMS/peak levels for VU meters (10fps)
 *   - status: Encoder state, stream/recorder statuses (3s)
 *   - config_changed: Signal to refetch /api/config
 *
 * WebSocket Commands (outgoing):
 *   - start, stop: Encoder control
 *   - recorders/start, recorders/stop: Recorder control
 *
 * Dependencies:
 *   - Alpine.js 3.x (loaded before this script)
 *   - icons.js (window.icons object for SVG rendering)
 *
 * @see index.html for markup structure
 * @see icons.js for SVG icon definitions
 */

// === Constants ===
const DB_MINIMUM = -60;           // Minimum dB level for VU meter range
const DB_RANGE = 60;              // dB range (0 to -60)
const CLIP_TIMEOUT_MS = 1500;     // Peak hold / clip indicator timeout
const WS_RECONNECT_MS = 1000;     // WebSocket reconnection delay
const TEST_FEEDBACK_MS = 2000;    // Test result display duration
const TOAST_DURATION_SUCCESS = 3000;  // Success toast auto-dismiss
const TOAST_DURATION_ERROR = 5000;    // Error toast auto-dismiss
const MAX_TOASTS = 3;             // Maximum visible toasts

// === PPM Ballistics ===
// IEC 60268-10 Type I: 20dB fallback in 1.7 seconds
const PPM_DECAY_DB_PER_SEC = 20 / 1.7;  // ~11.76 dB/sec
const PPM_FRAME_INTERVAL_MS = 100;       // 10 fps from WebSocket
const PPM_DECAY_PER_FRAME = PPM_DECAY_DB_PER_SEC * (PPM_FRAME_INTERVAL_MS / 1000);  // ~1.18 dB per frame

// === API Endpoints ===
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

/** Formats milliseconds to human-readable smart units (ms/s/m). */
const formatSmartDuration = (ms) => {
    if (ms < 1000) return `${Math.round(ms)}ms`;
    if (ms < 60000) {
        const sec = ms / 1000;
        return sec < 10 ? `${sec.toFixed(1)}s` : `${Math.round(sec)}s`;
    }
    const mins = Math.floor(ms / 60000);
    const secs = Math.round((ms % 60000) / 1000);
    return secs > 0 ? `${mins}m ${secs}s` : `${mins}m`;
};

const msToSeconds = (ms) => ms / 1000;
const secondsToMs = (sec) => Math.round(sec * 1000);

/** Converts dB (-60 to 0) to percentage (0-100) for VU meter display. */
window.dbToPercent = (db) => Math.max(0, Math.min(100, (db - DB_MINIMUM) / DB_RANGE * 100));

const DEFAULT_STREAM = {
    host: '',
    port: 8080,
    stream_id: '',
    password: '',
    codec: 'wav',
    max_retries: 99
};

const DEFAULT_RECORDER = {
    name: '',
    enabled: true,
    codec: 'mp3',
    rotation_mode: 'hourly',
    storage_mode: 'local',
    local_path: '',
    s3_endpoint: '',
    s3_bucket: '',
    s3_access_key_id: '',
    s3_secret_access_key: '',
    retention_days: 90
};

const DEFAULT_LEVELS = {
    left: -60,
    right: -60,
    peak_left: -60,
    peak_right: -60,
    silence_level: null,
    // PPM display levels (with ballistics applied)
    display_left: -60,
    display_right: -60,
    // Frontend peak hold (tracks highest display level)
    hold_left: -60,
    hold_right: -60,
    hold_left_time: 0,
    hold_right_time: 0
};

const deepClone = (obj) => JSON.parse(JSON.stringify(obj));

document.addEventListener('alpine:init', () => {
    Alpine.data('encoderApp', () => ({
        view: 'dashboard',
        settingsTab: 'audio',

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

        // Event history (all event types: stream_* and silence_*)
        events: [],
        eventFilter: '',
        eventsLoading: false,
        eventsHasMore: false,
        eventsOffset: 0,

        devices: [],
        levels: { ...DEFAULT_LEVELS },
        vuMode: localStorage.getItem('vuMode') || 'peak',
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
            silence_dump: { enabled: true, retention_days: 7 },
            webhook_url: '',
            zabbix_server: '',
            zabbix_port: 10051,
            zabbix_host: '',
            zabbix_key: '',
            graph_tenant_id: '',
            graph_client_id: '',
            graph_from_address: '',
            graph_recipients: '',
            graph_has_secret: false,
            recording_api_key: '',
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
            silenceDump: { enabled: true, retentionDays: 7 },
            silenceWebhook: '',
            zabbix: { server: '', port: 10051, host: '', key: '' },
            graph: { tenantId: '', clientId: '', clientSecret: '', fromAddress: '', recipients: '' },
            recordingApiKey: '',
            platform: ''
        },
        settingsDirty: false,
        saving: false,

        graphSecretExpiry: { expires_soon: false, days_left: 0 },

        version: { current: '', latest: '', update_available: false, commit: '', build_time: '' },

        ffmpegAvailable: true, // Assume available until we get status

        // Notification test state (unified object for all test types)
        testStates: {
            webhook: { pending: false, text: 'Test' },
            email: { pending: false, text: 'Test' },
            zabbix: { pending: false, text: 'Test' },
            recorderS3: { pending: false, text: 'Test Connection' }
        },

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

            const needsLocal = ['local', 'both'].includes(this.recorderForm.storage_mode);
            const needsS3 = ['s3', 'both'].includes(this.recorderForm.storage_mode);

            if (needsLocal && !this.recorderForm.local_path?.trim()) return false;
            if (needsS3) {
                if (!this.recorderForm.s3_bucket?.trim()) return false;
                if (!this.recorderForm.s3_access_key_id?.trim()) return false;
                // Secret only required for new recorders; edit mode keeps existing secret
                if (!this.isRecorderEditMode && !this.recorderForm.s3_secret_access_key) return false;
            }
            return true;
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
                const response = await fetch(API.CONFIG);
                if (!response.ok) {
                    throw new Error(`HTTP ${response.status}`);
                }
                const data = await response.json();
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
                const response = await fetch(API.DEVICES);
                if (response.ok) {
                    const data = await response.json();
                    this.devices = data.devices || [];
                }
            } catch {
                // Silently fail - devices will show stale data
            }
        },

        /** Global keyboard: Escape closes views, Enter saves, arrows navigate tabs. */
        handleGlobalKeydown(event) {
            // Don't handle if user is typing in an input field
            const isInput = ['INPUT', 'TEXTAREA', 'SELECT'].includes(event.target.tagName);

            // Escape: Close views/modals
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
            const peakHoldMs = 3000; // 3 second peak hold (broadcast industry standard)

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
            // FFmpeg availability
            this.ffmpegAvailable = msg.ffmpeg_available ?? true;

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
                    }, 400);
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
                silenceDump: {
                    enabled: this.config.silence_dump?.enabled ?? true,
                    retentionDays: this.config.silence_dump?.retention_days ?? 7
                },
                silenceWebhook: this.config.webhook_url || '',
                zabbix: {
                    server: this.config.zabbix_server || '',
                    port: this.config.zabbix_port || 10051,
                    host: this.config.zabbix_host || '',
                    key: this.config.zabbix_key || ''
                },
                graph: {
                    tenantId: this.config.graph_tenant_id || '',
                    clientId: this.config.graph_client_id || '',
                    clientSecret: '', // Never pre-fill, only send if user enters new value
                    fromAddress: this.config.graph_from_address || '',
                    recipients: this.config.graph_recipients || ''
                },
                recordingApiKey: this.config.recording_api_key || '',
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
                const response = await fetch(API.SETTINGS, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({
                        audio_input: this.config.audio_input,
                        silence_threshold: this.config.silence_threshold,
                        silence_duration_ms: this.config.silence_duration_ms,
                        silence_recovery_ms: this.config.silence_recovery_ms,
                        silence_dump_enabled: this.config.silence_dump.enabled,
                        silence_dump_retention_days: this.config.silence_dump.retention_days,
                        webhook_url: this.config.webhook_url,
                        zabbix_server: this.config.zabbix_server,
                        zabbix_port: this.config.zabbix_port,
                        zabbix_host: this.config.zabbix_host,
                        zabbix_key: this.config.zabbix_key,
                        graph_tenant_id: this.config.graph_tenant_id,
                        graph_client_id: this.config.graph_client_id,
                        graph_client_secret: '',
                        graph_from_address: this.config.graph_from_address,
                        graph_recipients: this.config.graph_recipients
                    })
                });
                if (!response.ok) {
                    const data = await response.json();
                    // Handle multiple errors array - show each as separate toast
                    if (data.errors && data.errors.length > 0) {
                        this.config.audio_input = prev;
                        for (const err of data.errors) {
                            this.showToast(err, 'error');
                        }
                        return;
                    }
                    throw new Error(data.error || `HTTP ${response.status}`);
                }
                this.showToast('Audio input updated', 'success');
            } catch (err) {
                this.config.audio_input = prev;
                this.showToast(`Failed to update audio input: ${err.message}`, 'error');
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
                silence_dump_enabled: form.silenceDump.enabled,
                silence_dump_retention_days: form.silenceDump.retentionDays,
                webhook_url: form.silenceWebhook,
                zabbix_server: form.zabbix.server,
                zabbix_port: form.zabbix.port,
                zabbix_host: form.zabbix.host,
                zabbix_key: form.zabbix.key,
                graph_tenant_id: form.graph.tenantId,
                graph_client_id: form.graph.clientId,
                graph_client_secret: form.graph.clientSecret || '', // empty = keep existing
                graph_from_address: form.graph.fromAddress,
                graph_recipients: form.graph.recipients
            };

            try {
                const response = await fetch(API.SETTINGS, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(payload)
                });

                if (!response.ok) {
                    const data = await response.json();
                    // Handle multiple errors array - show each as separate toast
                    if (data.errors && data.errors.length > 0) {
                        for (const err of data.errors) {
                            this.showToast(err, 'error');
                        }
                        return;
                    }
                    throw new Error(data.error || `HTTP ${response.status}`);
                }

                // Reload config in background to sync state
                this.loadConfig();
                this.showDashboard();
                this.showToast('Settings saved', 'success');
            } catch (err) {
                this.showToast(`Failed to save settings: ${err.message}`, 'error');
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
                    host: stream.host,
                    port: stream.port,
                    stream_id: stream.stream_id || '',
                    password: '',
                    codec: stream.codec || 'wav',
                    max_retries: stream.max_retries || 99,
                    enabled: stream.enabled !== false
                };
            } else {
                this.streamForm = { ...DEFAULT_STREAM, id: '', enabled: true };
            }
            this.streamFormDirty = false;
            this.view = 'stream-form';
        },

        showTab(tabId) {
            this.settingsTab = tabId;
        },

        // Stream management (REST API)

        /**
         * Submits stream form via REST API.
         */
        async submitStreamForm() {
            if (!this.streamForm.host?.trim()) return;

            const data = {
                host: this.streamForm.host.trim(),
                port: this.streamForm.port,
                stream_id: this.streamForm.stream_id.trim() || 'studio',
                codec: this.streamForm.codec,
                max_retries: this.streamForm.max_retries
            };

            if (this.streamForm.password) {
                data.password = this.streamForm.password;
            }

            try {
                let response;
                if (this.isEditMode) {
                    data.enabled = this.streamForm.enabled;
                    response = await fetch(`${API.STREAMS}/${this.streamForm.id}`, {
                        method: 'PUT',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify(data)
                    });
                } else {
                    response = await fetch(API.STREAMS, {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify(data)
                    });
                }

                if (!response.ok) {
                    const result = await response.json();
                    throw new Error(result.error || `HTTP ${response.status}`);
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
                        Object.assign(this.streams[index], data);
                    }
                }

                // Reload config in background to sync full state
                this.loadConfig();
                const toastMsg = this.isEditMode ? 'Stream updated' : 'Stream added';
                this.showDashboard();
                this.showToast(toastMsg, 'success');
            } catch (err) {
                this.showToast(`Failed to save stream: ${err.message}`, 'error');
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
                const response = await fetch(`${API.STREAMS}/${id}`, {
                    method: 'DELETE'
                });

                if (!response.ok) {
                    const result = await response.json();
                    throw new Error(result.error || `HTTP ${response.status}`);
                }

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
                    rotation_mode: recorder.rotation_mode || 'hourly',
                    storage_mode: recorder.storage_mode || 'local',
                    local_path: recorder.local_path || '',
                    s3_endpoint: recorder.s3_endpoint || '',
                    s3_bucket: recorder.s3_bucket || '',
                    s3_access_key_id: recorder.s3_access_key_id || '',
                    s3_secret_access_key: '',
                    retention_days: recorder.retention_days || 90
                };
            } else {
                this.recorderForm = { ...DEFAULT_RECORDER, id: '' };
            }
            this.recorderFormDirty = false;
            this.view = 'recorder-form';
        },

        markRecorderFormDirty() {
            this.recorderFormDirty = true;
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
            const needsLocal = storageMode === 'local' || storageMode === 'both';
            const needsS3 = storageMode === 's3' || storageMode === 'both';

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
                if (!this.isRecorderEditMode && !secretKey) {
                    this.showToast('S3 Secret Access Key is required', 'error');
                    return;
                }
            }

            const data = {
                name: name,
                enabled: this.recorderForm.enabled,
                codec: this.recorderForm.codec,
                rotation_mode: this.recorderForm.rotation_mode,
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
                let response;
                if (this.isRecorderEditMode) {
                    response = await fetch(`${API.RECORDERS}/${this.recorderForm.id}`, {
                        method: 'PUT',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify(data)
                    });
                } else {
                    response = await fetch(API.RECORDERS, {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify(data)
                    });
                }

                if (!response.ok) {
                    const result = await response.json();
                    throw new Error(result.error || `HTTP ${response.status}`);
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
                        Object.assign(this.recorders[index], data);
                    }
                }

                // Reload config in background to sync full state
                this.loadConfig();
                const toastMsg = this.isRecorderEditMode ? 'Recorder updated' : 'Recorder added';
                this.showDashboard();
                this.showToast(toastMsg, 'success');
            } catch (err) {
                this.showToast(`Failed to save recorder: ${err.message}`, 'error');
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
                const response = await fetch(`${API.RECORDERS}/${id}`, {
                    method: 'DELETE'
                });

                if (!response.ok) {
                    const result = await response.json();
                    throw new Error(result.error || `HTTP ${response.status}`);
                }

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
                const response = await fetch(`${API.RECORDERS}/${id}/${action}`, {
                    method: 'POST'
                });

                if (!response.ok) {
                    const result = await response.json();
                    throw new Error(result.error || `HTTP ${response.status}`);
                }
            } catch (err) {
                this.showToast(`Failed to ${action} recorder: ${err.message}`, 'error');
            }
        },

        /**
         * Tests S3 connection via REST API.
         */
        async testRecorderS3() {
            this.testStates.recorderS3 = { pending: true, text: 'Testing...' };

            try {
                const response = await fetch(API.RECORDERS_TEST_S3, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({
                        s3_endpoint: this.recorderForm.s3_endpoint,
                        s3_bucket: this.recorderForm.s3_bucket,
                        s3_access_key_id: this.recorderForm.s3_access_key_id,
                        s3_secret_access_key: this.recorderForm.s3_secret_access_key
                    })
                });

                const result = await response.json();

                this.testStates.recorderS3.pending = false;
                this.testStates.recorderS3.text = response.ok ? 'Connected!' : 'Failed';

                if (!response.ok) {
                    this.showToast(`S3 test failed: ${result.error || 'Unknown error'}`, 'error');
                }

                setTimeout(() => {
                    this.testStates.recorderS3.text = 'Test Connection';
                }, TEST_FEEDBACK_MS);
            } catch (err) {
                this.testStates.recorderS3.pending = false;
                this.testStates.recorderS3.text = 'Failed';
                this.showToast(`S3 test failed: ${err.message}`, 'error');
                setTimeout(() => {
                    this.testStates.recorderS3.text = 'Test Connection';
                }, TEST_FEEDBACK_MS);
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
                        statusText = 'Connecting...';
                        break;
                    case 'running':
                        if (status.stable) {
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
            localStorage.setItem('vuMode', this.vuMode);
        },

        resetVuMeter() {
            this.levels = { ...DEFAULT_LEVELS };
        },

        /**
         * Loads events from the API (all types: stream_* and silence_*).
         * @param {boolean} reset - If true, resets pagination and replaces events
         */
        async loadEvents(reset = true) {
            if (reset) {
                this.eventsOffset = 0;
                this.events = [];
            }
            this.eventsLoading = true;
            try {
                const params = new URLSearchParams({ limit: '50', offset: this.eventsOffset.toString() });
                if (this.eventFilter) {
                    params.set('type', this.eventFilter);
                }
                const response = await fetch(`/api/events?${params}`);
                if (response.ok) {
                    const data = await response.json();
                    const newEvents = (data.events || []).map(e => ({ ...e, expanded: false }));
                    if (reset) {
                        this.events = newEvents;
                    } else {
                        this.events = [...this.events, ...newEvents];
                    }
                    this.eventsHasMore = data.has_more || false;
                }
            } catch (error) {
                console.error('Failed to load events:', error);
            } finally {
                this.eventsLoading = false;
            }
        },

        /**
         * Loads more events (pagination).
         */
        async loadMoreEvents() {
            this.eventsOffset += 50;
            await this.loadEvents(false);
        },

        /**
         * Formats an event timestamp as short time (HH:MM:SS).
         * @param {string} ts - ISO timestamp
         * @returns {string} Formatted time string
         */
        formatEventTime(ts) {
            if (!ts) return '';
            const date = new Date(ts);
            return date.toLocaleTimeString('nl-NL', { hour: '2-digit', minute: '2-digit', second: '2-digit' });
        },

        /**
         * Returns a date key (YYYY-MM-DD) for grouping events by day.
         * @param {string} ts - ISO timestamp
         * @returns {string} Date key
         */
        getEventDateKey(ts) {
            if (!ts) return '';
            const d = new Date(ts);
            return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
        },

        /**
         * Checks whether the event at the given index starts a new date group.
         * @param {number} index - Event index in the events array
         * @returns {boolean} True if this event's date differs from the previous event
         */
        isNewDateGroup(index) {
            if (index === 0) return true;
            return this.getEventDateKey(this.events[index].ts) !== this.getEventDateKey(this.events[index - 1].ts);
        },

        /**
         * Formats a date label for date separator rows.
         * Returns "Today", "Yesterday", or a localized date string.
         * @param {string} ts - ISO timestamp
         * @returns {string} Human-readable date label
         */
        formatEventDate(ts) {
            if (!ts) return '';
            const date = new Date(ts);
            const now = new Date();
            const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
            const eventDay = new Date(date.getFullYear(), date.getMonth(), date.getDate());
            const diffDays = Math.round((today - eventDay) / 86400000);

            if (diffDays === 0) return 'Today';
            if (diffDays === 1) return 'Yesterday';
            return date.toLocaleDateString('nl-NL', { weekday: 'short', day: 'numeric', month: 'short', year: 'numeric' });
        },

        /**
         * Formats a timestamp as relative time (e.g., "2m ago", "1h ago").
         * @param {string} ts - ISO timestamp
         * @returns {string} Relative time string
         */
        formatRelativeTime(ts) {
            if (!ts) return '';
            const now = Date.now();
            const then = new Date(ts).getTime();
            const diff = now - then;

            const seconds = Math.floor(diff / 1000);
            const minutes = Math.floor(seconds / 60);
            const hours = Math.floor(minutes / 60);
            const days = Math.floor(hours / 24);

            if (seconds < 60) return 'just now';
            if (minutes < 60) return `${minutes}m ago`;
            if (hours < 24) return `${hours}h ago`;
            if (days === 1) return 'yesterday';
            return `${days}d ago`;
        },

        /**
         * Gets the severity level for an event type.
         * @param {string} type - Event type
         * @returns {string} Severity: 'error', 'warning', 'success', or 'info'
         */
        getEventSeverity(type) {
            if (type === 'stream_error') return 'error';
            if (type === 'stream_retry') return 'warning';
            if (type === 'stream_stable') return 'success';
            if (type === 'silence_start') return 'warning';
            if (type === 'silence_end') return 'success';
            if (type === 'recorder_error' || type === 'upload_failed' || type === 'upload_abandoned') return 'error';
            if (type === 'upload_completed' || type === 'cleanup_completed') return 'success';
            if (type === 'upload_retry') return 'warning';
            return 'info';
        },

        /**
         * Gets a short label for an event type.
         * @param {string} type - Event type
         * @returns {string} Short label
         */
        getEventLabel(type) {
            const labels = {
                'stream_started': 'Started',
                'stream_stable': 'Connected',
                'stream_error': 'Error',
                'stream_retry': 'Retry',
                'stream_stopped': 'Stopped',
                'silence_start': 'Silence',
                'silence_end': 'Recovered',
                'recorder_started': 'Started',
                'recorder_stopped': 'Stopped',
                'recorder_error': 'Error',
                'recorder_file': 'New File',
                'upload_queued': 'Upload Queued',
                'upload_completed': 'Uploaded',
                'upload_failed': 'Upload Failed',
                'upload_retry': 'Retry',
                'upload_abandoned': 'Abandoned',
                'cleanup_completed': 'Cleanup'
            };
            return labels[type] || type;
        },

        /**
         * Gets the detail text for an event (error message, stream name, etc.).
         * @param {Object} event - Event object
         * @returns {string} Detail text
         */
        getEventDetail(event) {
            const details = event.details || {};
            const streamName = details.stream_name || '';

            if (event.type === 'stream_error') {
                return details.error || 'Unknown error';
            }
            if (event.type === 'stream_retry') {
                const retryNum = details.retry ? `Retry #${details.retry}` : '';
                const error = details.error || '';
                return [retryNum, error].filter(Boolean).join(' — ');
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
            // Recorder events
            if (event.type === 'recorder_error' || event.type === 'upload_failed') {
                return details.error || 'Unknown error';
            }
            if (event.type === 'recorder_file' || event.type === 'upload_queued' || event.type === 'upload_completed') {
                const filename = details.filename || '';
                const codec = details.codec || '';
                return [filename, codec.toUpperCase()].filter(Boolean).join(' — ');
            }
            if (event.type === 'cleanup_completed') {
                const count = details.files_deleted || 0;
                const storage = details.storage_type || '';
                return `${count} files deleted (${storage})`;
            }
            if (event.type === 'recorder_started' || event.type === 'recorder_stopped') {
                const codec = details.codec ? details.codec.toUpperCase() : '';
                const mode = details.storage_mode || '';
                const modeLabel = mode === 'both' ? 'Local + S3' : mode === 's3' ? 'S3' : mode === 'local' ? 'Local' : '';
                return [codec, modeLabel].filter(Boolean).join(' — ');
            }
            // For started/stable/stopped, show stream name
            return streamName;
        },

        /**
         * Gets the text for stream badge.
         * @param {Object} event - Event object
         * @returns {string} Short stream identifier
         */
        getStreamBadgeText(event) {
            // Silence events show "Audio" (they're system-wide, not stream-specific)
            if (event.type?.startsWith('silence_')) {
                return 'Audio';
            }
            // Recorder events show recorder name
            if (event.type?.startsWith('recorder_') || event.type?.startsWith('upload_') || event.type === 'cleanup_completed') {
                const details = event.details || {};
                const name = details.recorder_name || 'Recorder';
                return name.length > 8 ? name.slice(0, 8) : name;
            }
            // Stream events show stream name (shortened if needed)
            const details = event.details || {};
            const name = details.stream_name || event.stream_id || 'Stream';
            // Return first part if it's a compound name, or truncate
            return name.length > 8 ? name.slice(0, 8) : name;
        },

        /**
         * Gets the event category for styling.
         * @param {Object} event - Event object
         * @returns {string} Category: 'stream', 'audio', 'recorder', or 'system'
         */
        getEventCategory(event) {
            if (event.type?.startsWith('silence_')) return 'audio';
            if (event.type?.startsWith('recorder_') || event.type?.startsWith('upload_')) return 'recorder';
            if (event.type?.startsWith('stream_')) return 'stream';
            return 'system';
        },

        /**
         * Gets the short badge letter for event category.
         * @param {Object} event - Event object
         * @returns {string} Single letter: 'S', 'A', 'R', or '•'
         */
        getEventCategoryBadge(event) {
            const category = this.getEventCategory(event);
            if (category === 'stream') return 'S';
            if (category === 'audio') return 'A';
            if (category === 'recorder') return 'R';
            return '•';
        },

        /**
         * Triggers a notification test via REST API.
         * Uses form values (unsaved) for testing.
         *
         * @param {string} type - Test type: 'webhook', 'log', 'email', or 'zabbix'
         */
        async sendTest(type) {
            if (!this.testStates[type]) return;
            this.testStates[type].pending = true;
            this.testStates[type].text = 'Testing...';

            // Build payload with current form values
            const payload = {};
            if (this.settingsForm) {
                if (type === 'webhook') {
                    payload.webhook_url = this.settingsForm.silenceWebhook;
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
                    payload.zabbix_key = this.settingsForm.zabbix.key;
                }
            }

            try {
                const response = await fetch(`${API.NOTIFICATIONS_TEST}/${type}`, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(payload)
                });

                const result = await response.json();

                this.testStates[type].pending = false;
                this.testStates[type].text = response.ok ? 'Sent!' : 'Failed';

                if (!response.ok) {
                    const typeName = type.charAt(0).toUpperCase() + type.slice(1);
                    this.showToast(`${typeName} test failed: ${result.error || 'Unknown error'}`, 'error');
                }

                setTimeout(() => { this.testStates[type].text = 'Test'; }, TEST_FEEDBACK_MS);
            } catch (err) {
                this.testStates[type].pending = false;
                this.testStates[type].text = 'Failed';
                this.showToast(`Test failed: ${err.message}`, 'error');
                setTimeout(() => { this.testStates[type].text = 'Test'; }, TEST_FEEDBACK_MS);
            }
        },


        /**
         * Regenerates the API key for recording endpoints via REST API.
         */
        async regenerateApiKey() {
            if (!confirm('Regenerate API key? Existing integrations will stop working.')) return;

            try {
                const response = await fetch(API.RECORDING_REGENERATE_KEY, {
                    method: 'POST'
                });

                const result = await response.json();

                if (!response.ok) {
                    throw new Error(result.error || `HTTP ${response.status}`);
                }

                if (result.api_key) {
                    // Update form if open - field update is the feedback
                    if (this.settingsForm) {
                        this.settingsForm.recordingApiKey = result.api_key;
                    }
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
            const key = this.settingsForm?.recordingApiKey || this.config.recording_api_key;
            try {
                await navigator.clipboard.writeText(key);
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
                this._bannerTimeout = setTimeout(() => this.hideBanner(), 10000);
            }
        },

        hideBanner() {
            if (this._bannerTimeout) {
                clearTimeout(this._bannerTimeout);
                this._bannerTimeout = null;
            }
            this.banner.visible = false;
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
