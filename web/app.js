/**
 * ZuidWest FM Encoder - Alpine.js Web Application
 *
 * Real-time audio monitoring, encoder control, and multi-output stream
 * management via WebSocket connection to Go backend.
 *
 * Architecture:
 *   - Single Alpine.js component (encoderApp) manages all UI state
 *   - WebSocket connection at /ws for bidirectional communication
 *   - Views: dashboard, settings, output-form, recorder-form
 *
 * WebSocket Message Types (incoming):
 *   - levels: Audio RMS/peak levels for VU meters
 *   - status: Encoder state, outputs, recorders, devices, settings (every 3s)
 *   - test_result: Notification test result with test_type field
 *
 * WebSocket Commands (outgoing):
 *   - audio/update, silence/update: Update audio/silence settings
 *   - notifications/{type}/update, notifications/{type}/test: Notification config and tests
 *   - outputs/add, outputs/delete, outputs/update: Manage stream outputs
 *   - recorders/add, recorders/delete, recorders/update: Manage recorders
 *   - recorders/start, recorders/stop: Control on-demand recorders
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

/**
 * Formats milliseconds to human-readable smart units.
 * - <1000ms: shows as "XXXms"
 * - <60000ms: shows as "Xs" or "X.Xs"
 * - >=60000ms: shows as "Xm Ys"
 *
 * @param {number} ms - Duration in milliseconds
 * @returns {string} Formatted duration
 */
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

/**
 * Converts milliseconds to seconds for UI display/input.
 * @param {number} ms - Duration in milliseconds
 * @returns {number} Duration in seconds
 */
const msToSeconds = (ms) => ms / 1000;

/**
 * Converts seconds to milliseconds for storage.
 * @param {number} sec - Duration in seconds
 * @returns {number} Duration in milliseconds
 */
const secondsToMs = (sec) => Math.round(sec * 1000);

/**
 * Converts decibel value to percentage for VU meter display.
 * Maps -60dB to 0% and 0dB to 100%.
 *
 * @param {number} db - Decibel value (typically -60 to 0)
 * @returns {number} Percentage value (0-100), clamped to valid range
 */
window.dbToPercent = (db) => Math.max(0, Math.min(100, (db - DB_MINIMUM) / DB_RANGE * 100));

const DEFAULT_OUTPUT = {
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
    silence_level: null
};

/**
 * Creates a deep clone of an object using JSON serialization.
 * Safe for plain objects without functions, undefined, or circular refs.
 *
 * @param {Object} obj - Object to clone
 * @returns {Object} Deep copy of the object
 */
const deepClone = (obj) => JSON.parse(JSON.stringify(obj));

document.addEventListener('alpine:init', () => {
    Alpine.data('encoderApp', () => ({
        view: 'dashboard',
        settingsTab: 'audio',

        vuChannels: [
            { label: 'L', level: 'left', peak: 'peak_left' },
            { label: 'R', level: 'right', peak: 'peak_right' }
        ],

        settingsTabs: [
            { id: 'audio', label: 'Audio', icon: 'audio' },
            { id: 'notifications', label: 'Notifications', icon: 'email' },
            { id: 'recording', label: 'Recording', icon: 'microphone' },
            { id: 'about', label: 'About', icon: 'info' }
        ],

        outputForm: { ...DEFAULT_OUTPUT, id: '', enabled: true },
        outputFormDirty: false,

        encoder: {
            state: 'connecting',
            uptime: '',
            sourceRetryCount: 0,
            sourceMaxRetries: 10,
            lastError: ''
        },

        outputs: [],
        outputStatuses: {},
        previousOutputStatuses: {},
        deletingOutputs: {},
        connectingAnimations: {},  // Track output connection animations reactively

        recorders: [],
        recorderStatuses: {},
        deletingRecorders: {},
        recorderForm: { ...DEFAULT_RECORDER, id: '' },
        recorderFormDirty: false,

        devices: [],
        levels: { ...DEFAULT_LEVELS },
        vuMode: localStorage.getItem('vuMode') || 'peak',
        clipActive: false,
        clipTimeout: null,

        settings: {
            audioInput: '',
            silenceThreshold: -40,
            silenceDuration: 15,
            silenceRecovery: 5,
            silenceWebhook: '',
            silenceLogPath: '',
            graph: { tenantId: '', clientId: '', clientSecret: '', fromAddress: '', recipients: '' },
            recordingApiKey: '',
            platform: ''
        },
        originalSettings: null,
        settingsDirty: false,
        formErrors: {},  // Field-level validation errors keyed by field path

        graphSecretExpiry: { expires_soon: false, days_left: 0 },

        version: { current: '', latest: '', update_available: false, commit: '', build_time: '' },

        ffmpegAvailable: true, // Assume available until we get status

        // Notification test state (unified object for all test types)
        testStates: {
            webhook: { pending: false, text: 'Test' },
            log: { pending: false, text: 'Test' },
            email: { pending: false, text: 'Test' },
            recorderS3: { pending: false, text: 'Test Connection' }
        },

        silenceLogModal: {
            visible: false,
            loading: false,
            entries: [],
            path: '',
            error: ''
        },

        banner: {
            visible: false,
            message: '',
            type: 'info', // info, warning, danger
            persistent: false
        },

        ws: null,
        _keydownHandler: null,
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
            return this.outputForm.id !== '';
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
                if (!this.isRecorderEditMode && !this.recorderForm.s3_secret_access_key) return false;
            }
            return true;
        },

        // Lifecycle
        /**
         * Alpine.js lifecycle hook - initializes WebSocket connection.
         * Called automatically when component mounts.
         */
        init() {
            this.connectWebSocket();
            // Global keyboard handlers - store reference for cleanup
            this._keydownHandler = (e) => this.handleGlobalKeydown(e);
            document.addEventListener('keydown', this._keydownHandler);
        },

        /**
         * Handles global keyboard events for navigation and actions.
         * - Escape: Close settings/add-output views, close silence log modal
         * - Enter: Save settings when on settings view (if dirty)
         * - Arrow keys: Navigate between settings tabs
         *
         * @param {KeyboardEvent} event - The keyboard event
         */
        handleGlobalKeydown(event) {
            // Don't handle if user is typing in an input field
            const isInput = ['INPUT', 'TEXTAREA', 'SELECT'].includes(event.target.tagName);

            // Escape: Close views/modals
            if (event.key === 'Escape') {
                if (this.silenceLogModal.visible) {
                    this.handleSilenceLogClose();
                    event.preventDefault();
                } else if (this.view === 'settings') {
                    this.cancelSettings();
                    event.preventDefault();
                } else if (this.view === 'output-form' || this.view === 'recorder-form') {
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

        /**
         * Establishes WebSocket connection to backend.
         * Handles incoming messages by type and auto-reconnects on close.
         * Reconnection uses WS_RECONNECT_MS delay to prevent rapid retries.
         */
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
                } else if (msg.type === 'test_result') {
                    this.handleTestResult(msg);
                } else if (msg.type === 'silence_log_result') {
                    this.handleSilenceLogResult(msg);
                } else if (msg.type === 'recorder_s3_test_result') {
                    this.handleRecorderS3TestResult(msg);
                } else if (msg.type === 'recorder_result') {
                    this.handleRecorderResult(msg);
                } else if (msg.type === 'output_result') {
                    this.handleOutputResult(msg);
                } else if (msg.type === 'recording/regenerate-key_result') {
                    if (msg.success && msg.data?.api_key) {
                        this.settings.recordingApiKey = msg.data.api_key;
                        this.showBanner('API key regenerated successfully', 'info');
                    } else if (!msg.success) {
                        this.showBanner(`Failed to regenerate API key: ${msg.error}`, 'danger');
                    }
                } else if (msg.type.endsWith('_result')) {
                    this.handleCommandResult(msg);
                }
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
         * Sends command to backend via WebSocket.
         *
         * @param {string} type - Command type (start, stop, outputs/add, silence/update, etc.)
         * @param {string} [id] - Optional entity ID for entity-specific commands
         * @param {Object} [data] - Optional payload data
         */
        send(type, id, data) {
            if (this.ws && this.ws.readyState === WebSocket.OPEN) {
                this.ws.send(JSON.stringify({ type: type, id: id, data: data }));
            }
        },

        /**
         * Processes incoming audio level data.
         * Updates VU meter display and manages clip detection state.
         * Clip indicator activates when levels exceed threshold and holds
         * for CLIP_TIMEOUT_MS before auto-clearing.
         *
         * @param {Object} levels - Audio levels (left, right, peak_left, peak_right, silence_level)
         */
        handleLevels(levels) {
            const prevSilenceState = this.getSilenceState();
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
            const thresholdMs = secondsToMs(this.settings.silenceDuration || 15);
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
         * Updates encoder state, output statuses, available devices, and settings.
         * Settings sync is skipped when user is on settings view to prevent
         * overwriting in-progress edits.
         *
         * @param {Object} msg - Status message with state, outputs, devices, settings
         */
        handleStatus(msg) {
            // FFmpeg availability
            this.ffmpegAvailable = msg.ffmpeg_available ?? true;

            this.encoder.state = msg.encoder.state;
            this.encoder.uptime = msg.encoder.uptime || '';
            this.encoder.sourceRetryCount = msg.encoder.source_retry_count || 0;
            this.encoder.sourceMaxRetries = msg.encoder.source_max_retries || 10;
            this.encoder.lastError = msg.encoder.last_error || '';

            if (!this.encoderRunning) {
                this.resetVuMeter();
            }

            this.outputs = msg.outputs || [];
            const newOutputStatuses = msg.output_status || {};

            // Detect status transitions to "connected" and trigger animation
            for (const id in newOutputStatuses) {
                const oldStatus = this.previousOutputStatuses[id] || {};
                const newStatus = newOutputStatuses[id] || {};

                // Check if status just became stable (connected)
                const wasNotConnected = !oldStatus.stable;
                const isNowConnected = newStatus.stable;

                if (wasNotConnected && isNowConnected) {
                    // Trigger animation via reactive state instead of DOM manipulation
                    this.connectingAnimations[id] = true;
                    setTimeout(() => {
                        delete this.connectingAnimations[id];
                    }, 400);
                }
            }

            this.previousOutputStatuses = deepClone(newOutputStatuses);
            this.outputStatuses = newOutputStatuses;

            for (const id in this.deletingOutputs) {
                const output = this.outputs.find(o => o.id === id);
                if (!output || output.created_at !== this.deletingOutputs[id]) {
                    delete this.deletingOutputs[id];
                }
            }

            // Only update settings and devices from status when not on settings view
            // to prevent overwriting user input while editing
            if (this.view !== 'settings') {
                // Devices (frozen while in settings to keep dropdown stable)
                if (msg.devices) {
                    this.devices = msg.devices;
                }
                if (msg.settings?.audio_input) {
                    this.settings.audioInput = msg.settings.audio_input;
                }
                if (msg.settings?.platform !== undefined) {
                    this.settings.platform = msg.settings.platform;
                }
                // Silence detection settings (convert ms to seconds for UI)
                this.settings.silenceThreshold = msg.silence_threshold ?? -40;
                this.settings.silenceDuration = msToSeconds(msg.silence_duration_ms ?? 15000);
                this.settings.silenceRecovery = msToSeconds(msg.silence_recovery_ms ?? 5000);
                this.settings.silenceWebhook = msg.silence_webhook ?? '';
                this.settings.silenceLogPath = msg.silence_log_path ?? '';
                // Microsoft Graph settings
                this.settings.graph.tenantId = msg.graph_tenant_id ?? '';
                this.settings.graph.clientId = msg.graph_client_id ?? '';
                this.settings.graph.fromAddress = msg.graph_from_address ?? '';
                this.settings.graph.recipients = msg.graph_recipients ?? '';
                // Update secret expiry info (only expires_soon and days_left are used in UI)
                this.graphSecretExpiry = msg.graph_secret_expiry ?? { expires_soon: false, days_left: 0 };
            }

            if (msg.version) {
                const wasUpdateAvail = this.version.update_available;
                this.version = msg.version;
                // Show banner once when update becomes available
                if (msg.version.update_available && !wasUpdateAvail) {
                    this.showBanner(`Update available: ${msg.version.latest}`, 'info', false);
                }
            }

            // Sync recorders and statuses
            this.recorders = msg.recorders || [];
            this.recorderStatuses = msg.recorder_statuses || {};
            // Only sync API key when not in settings view
            if (this.view !== 'settings') {
                this.settings.recordingApiKey = msg.recording_api_key || '';
            }

            // Clean up deleting recorders that are no longer in the list
            for (const id in this.deletingRecorders) {
                const recorder = this.recorders.find(r => r.id === id);
                if (!recorder || recorder.created_at !== this.deletingRecorders[id]) {
                    delete this.deletingRecorders[id];
                }
            }
        },

        /**
         * Handles notification test result from backend.
         * Updates UI feedback and auto-clears after TEST_FEEDBACK_MS.
         *
         * @param {Object} msg - Result with test_type, success, and optional error
         */
        handleTestResult(msg) {
            const type = msg.test_type;
            if (!Object.hasOwn(this.testStates, type)) return;

            this.testStates[type].pending = false;
            this.testStates[type].text = msg.success ? 'Sent!' : 'Failed';
            if (!msg.success) {
                const typeName = type.charAt(0).toUpperCase() + type.slice(1);
                this.showBanner(`${typeName} test failed: ${msg.error || 'Unknown error'}`, 'danger', false);
            }
            setTimeout(() => { this.testStates[type].text = 'Test'; }, TEST_FEEDBACK_MS);
        },

        // Navigation
        /**
         * Returns to dashboard view and clears settings state.
         */
        showDashboard() {
            this.view = 'dashboard';
            this.settingsDirty = false;
            this.originalSettings = null;
        },

        /**
         * Shows success state on save button, then navigates to dashboard.
         */
        saveAndClose() {
            const viewIds = {
                'settings': 'settings-view',
                'output-form': 'output-form-view'
            };
            const viewId = viewIds[this.view] || 'settings-view';
            const saveBtn = document.querySelector(`#${viewId} .nav-btn[data-variant="save"]`);
            if (saveBtn) {
                saveBtn.dataset.saved = 'true';
                setTimeout(() => {
                    delete saveBtn.dataset.saved;
                    this.showDashboard();
                }, 600);
            } else {
                this.showDashboard();
            }
        },

        /**
         * Navigates to settings view and captures current settings snapshot.
         * Snapshot enables cancel/restore functionality.
         */
        showSettings() {
            this.originalSettings = deepClone(this.settings);
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
         * Clears all form errors.
         */
        clearFormErrors() {
            this.formErrors = {};
        },

        /**
         * Checks if a field has an error.
         * @param {string} field - Field path (e.g., 'silence.threshold_db')
         * @returns {boolean} True if field has error
         */
        hasFieldError(field) {
            return !!this.formErrors[field];
        },

        /**
         * Gets error message for a field.
         * @param {string} field - Field path
         * @returns {string} Error message or empty string
         */
        getFieldError(field) {
            return this.formErrors[field] || '';
        },

        /**
         * Reverts settings to snapshot taken when entering settings view.
         * Returns to dashboard without saving changes.
         */
        cancelSettings() {
            if (this.originalSettings) {
                this.settings = deepClone(this.originalSettings);
            }
            this.showDashboard();
        },

        /**
         * Persists all settings to backend via WebSocket.
         * Sends separate commands for each settings category.
         * Resets dirty state on send.
         */
        saveSettings() {
            // Silence detection settings
            this.send('silence/update', null, {
                threshold_db: this.settings.silenceThreshold,
                duration_ms: secondsToMs(this.settings.silenceDuration),
                recovery_ms: secondsToMs(this.settings.silenceRecovery)
            });

            // Webhook notification settings
            this.send('notifications/webhook/update', null, {
                url: this.settings.silenceWebhook
            });

            // Log notification settings
            this.send('notifications/log/update', null, {
                path: this.settings.silenceLogPath
            });

            // Email notification settings (Microsoft Graph)
            const emailUpdate = {
                tenant_id: this.settings.graph.tenantId,
                client_id: this.settings.graph.clientId,
                from_address: this.settings.graph.fromAddress,
                recipients: this.settings.graph.recipients
            };
            // Only include client secret if it was changed
            if (this.settings.graph.clientSecret) {
                emailUpdate.client_secret = this.settings.graph.clientSecret;
            }
            this.send('notifications/email/update', null, emailUpdate);

            this.saveAndClose();
        },

        showOutputForm(id = null) {
            if (id) {
                const output = this.outputs.find(o => o.id === id);
                if (!output) return;
                this.outputForm = {
                    id: output.id,
                    host: output.host,
                    port: output.port,
                    stream_id: output.stream_id || '',
                    password: '',
                    codec: output.codec || 'wav',
                    max_retries: output.max_retries || 99,
                    enabled: output.enabled !== false
                };
            } else {
                this.outputForm = { ...DEFAULT_OUTPUT, id: '', enabled: true };
            }
            this.outputFormDirty = false;
            this.view = 'output-form';
        },

        /**
         * Switches active settings tab.
         * @param {string} tabId - Tab identifier (audio, notifications, recording, about)
         */
        showTab(tabId) {
            this.settingsTab = tabId;
        },

        // Output management
        submitOutputForm() {
            if (!this.outputForm.host?.trim()) return;

            const data = {
                host: this.outputForm.host.trim(),
                port: this.outputForm.port,
                stream_id: this.outputForm.stream_id.trim() || 'studio',
                codec: this.outputForm.codec,
                max_retries: this.outputForm.max_retries
            };

            if (this.outputForm.password) {
                data.password = this.outputForm.password;
            }

            if (this.isEditMode) {
                data.enabled = this.outputForm.enabled;
                this.send('outputs/update', this.outputForm.id, data);
            } else {
                this.send('outputs/add', null, data);
            }
            this.saveAndClose();
        },

        /**
         * Initiates output deletion with confirmation and optimistic UI update.
         * @param {string} id - Output ID to delete
         * @param {boolean} [returnToDashboard=false] - Navigate to dashboard after delete
         */
        deleteOutput(id, returnToDashboard = false) {
            if (!confirm('Delete this output? This action cannot be undone.')) return;
            const output = this.outputs.find(o => o.id === id);
            if (output) this.deletingOutputs[id] = output.created_at;
            this.send('outputs/delete', id, null);
            if (returnToDashboard) this.showDashboard();
        },

        markOutputFormDirty() {
            this.outputFormDirty = true;
        },

        // Recorder management

        /**
         * Shows recorder form for adding or editing a recorder.
         * @param {string|null} id - Recorder ID for editing, null for new
         */
        showRecorderForm(id = null) {
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
         * Submits recorder form for add or update.
         */
        submitRecorderForm() {
            const name = this.recorderForm.name?.trim();
            const storageMode = this.recorderForm.storage_mode;
            const localPath = this.recorderForm.local_path?.trim();
            const bucket = this.recorderForm.s3_bucket?.trim();
            const accessKey = this.recorderForm.s3_access_key_id?.trim();
            const secretKey = this.recorderForm.s3_secret_access_key;

            // Validate required fields
            if (!name) {
                this.showBanner('Name is required', 'danger', true);
                return;
            }

            // Validate based on storage mode
            const needsLocal = storageMode === 'local' || storageMode === 'both';
            const needsS3 = storageMode === 's3' || storageMode === 'both';

            if (needsLocal && !localPath) {
                this.showBanner('Local Path is required', 'danger', true);
                return;
            }
            if (needsS3) {
                if (!bucket) {
                    this.showBanner('S3 Bucket is required', 'danger', true);
                    return;
                }
                if (!accessKey) {
                    this.showBanner('S3 Access Key ID is required', 'danger', true);
                    return;
                }
                // Secret key required for new recorders
                if (!this.isRecorderEditMode && !secretKey) {
                    this.showBanner('S3 Secret Access Key is required', 'danger', true);
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

            if (this.isRecorderEditMode) {
                this.send('recorders/update', this.recorderForm.id, data);
            } else {
                this.send('recorders/add', null, data);
            }
            // Navigation handled by handleRecorderResult on success
        },

        /**
         * Deletes a recorder with confirmation.
         * @param {string} id - Recorder ID to delete
         * @param {boolean} returnToDashboard - Navigate to dashboard after delete
         */
        deleteRecorder(id, returnToDashboard = false) {
            if (!confirm('Delete this recorder? This action cannot be undone.')) return;
            const recorder = this.recorders.find(r => r.id === id);
            if (recorder) this.deletingRecorders[id] = recorder.created_at;
            this.send('recorders/delete', id, null);
            if (returnToDashboard) this.showDashboard();
        },

        /**
         * Starts recording for a specific recorder.
         * @param {string} id - Recorder ID
         */
        startRecorder(id) {
            this.send('recorders/start', id, null);
        },

        /**
         * Stops recording for a specific recorder.
         * @param {string} id - Recorder ID
         */
        stopRecorder(id) {
            this.send('recorders/stop', id, null);
        },

        /**
         * Tests S3 connection for the current recorder form.
         */
        testRecorderS3() {
            this.testStates.recorderS3 = { pending: true, text: 'Testing...' };
            this.send('recorders/test-s3', null, {
                s3_endpoint: this.recorderForm.s3_endpoint,
                s3_bucket: this.recorderForm.s3_bucket,
                s3_access_key_id: this.recorderForm.s3_access_key_id,
                s3_secret_access_key: this.recorderForm.s3_secret_access_key
            });
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
         * Computes all display data for an output in a single call.
         * Use this method to avoid multiple getOutputStatus() calls per render.
         *
         * @param {Object} output - Output object with id and created_at
         * @returns {Object} Object with stateClass, statusText, showError, and lastError
         */
        getOutputDisplayData(output) {
            const status = this.outputStatuses[output.id] || {};
            const isDeleting = this.deletingOutputs[output.id] === output.created_at;

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
                            statusText = 'Connected';
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

        /**
         * Toggles VU meter display mode between peak and RMS.
         */
        toggleVuMode() {
            this.vuMode = this.vuMode === 'peak' ? 'rms' : 'peak';
            localStorage.setItem('vuMode', this.vuMode);
        },

        /**
         * Resets VU meter to default zero state.
         * Called when encoder stops or on initialization.
         */
        resetVuMeter() {
            this.levels = { ...DEFAULT_LEVELS };
        },

        /**
         * Triggers a notification test via WebSocket.
         * Temporarily disables button and shows testing state.
         *
         * @param {string} type - Test type: 'webhook', 'log', or 'email'
         */
        sendTest(type) {
            if (!this.testStates[type]) return;
            this.testStates[type].pending = true;
            this.testStates[type].text = 'Testing...';
            this.send(`notifications/${type}/test`, null, null);
        },

        /**
         * Handles recorder S3 connection test result.
         * @param {Object} msg - Result with success, error
         */
        handleRecorderS3TestResult(msg) {
            this.testStates.recorderS3.pending = false;
            this.testStates.recorderS3.text = msg.success ? 'Connected!' : 'Failed';

            if (!msg.success) {
                this.showBanner(`S3 test failed: ${msg.error || 'Unknown error'}`, 'danger', false);
            }

            setTimeout(() => {
                this.testStates.recorderS3.text = 'Test Connection';
            }, TEST_FEEDBACK_MS);
        },

        /**
         * Handles recorder operation results (add, update, delete, start, stop).
         * @param {Object} msg - Result with action, success, error
         */
        handleRecorderResult(msg) {
            if (!msg.success) {
                this.showBanner(`Recorder ${msg.action} failed: ${msg.error || 'Unknown error'}`, 'danger', false);
                return;
            }

            // On successful add/update, go back to dashboard
            if (msg.action === 'add' || msg.action === 'update') {
                this.view = 'dashboard';
                this.recorderFormDirty = false;
            }
        },

        /**
         * Handles output operation results (add, update, delete, clear_error).
         * @param {Object} msg - Result with action, success, error
         */
        handleOutputResult(msg) {
            if (!msg.success) {
                const actionText = msg.action.replace('_', ' ');
                this.showBanner(`Output ${actionText} failed: ${msg.error || 'Unknown error'}`, 'danger', false);
                return;
            }

            // On successful add/update from output form, go back to dashboard
            if (this.view === 'output-form' && (msg.action === 'add' || msg.action === 'update')) {
                this.view = 'dashboard';
                this.outputFormDirty = false;
            }
        },

        /**
         * Handles command result messages (slash-style API responses).
         * Extracts field-level errors and displays them on the form.
         * @param {Object} msg - Result with type, success, error, data
         */
        handleCommandResult(msg) {
            // Clear previous errors on success
            if (msg.success) {
                this.clearFormErrors();
                return;
            }

            // Handle validation errors with field-level detail
            if (msg.error?.errors) {
                this.clearFormErrors();
                for (const fieldError of msg.error.errors) {
                    this.formErrors[fieldError.field] = fieldError.message;
                }
                // Show first error in banner
                const firstError = msg.error.errors[0];
                if (firstError) {
                    this.showBanner(firstError.message, 'danger', false);
                }
            } else if (msg.error) {
                // Simple error string
                this.showBanner(msg.error, 'danger', false);
            }
        },

        /**
         * Regenerates the API key for recording endpoints.
         * Calls backend to generate and persist new key.
         */
        regenerateApiKey() {
            if (!confirm('Regenerate API key? Existing integrations will need to be updated.')) return;
            this.send('recording/regenerate-key');
        },

        /**
         * Copies the API key to clipboard.
         */
        async copyApiKey() {
            try {
                await navigator.clipboard.writeText(this.settings.recordingApiKey);
                this.showBanner('API key copied to clipboard', 'info', false);
            } catch {
                this.showBanner('Failed to copy to clipboard', 'danger', false);
            }
        },

        /**
         * Handles silence log view result from backend.
         * Updates modal state with log entries or error message.
         *
         * @param {Object} msg - Result with success, entries[], path, error
         */
        handleSilenceLogResult(msg) {
            this.silenceLogModal.loading = false;
            if (msg.success) {
                this.silenceLogModal.entries = msg.entries || [];
                this.silenceLogModal.path = msg.path || '';
                this.silenceLogModal.error = '';
            } else {
                this.silenceLogModal.entries = [];
                this.silenceLogModal.error = msg.error || 'Unknown error';
            }
        },

        /**
         * Opens the silence log modal and fetches log entries.
         */
        viewSilenceLog() {
            this.silenceLogModal.visible = true;
            this.silenceLogModal.loading = true;
            this.silenceLogModal.entries = [];
            this.silenceLogModal.error = '';
            this.send('notifications/log/view', null, null);
        },

        handleSilenceLogClose() {
            this.silenceLogModal.visible = false;
        },

        handleSilenceLogRefresh() {
            this.silenceLogModal.loading = true;
            this.send('notifications/log/view', null, null);
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
         * Formats a silence log entry for display.
         * For "ended" events, duration is the key metric (total silence time).
         * For "started" events, duration is just detection delay (not shown).
         * @param {Object} entry - Log entry with timestamp, event, duration_ms, threshold_db, level_left_db, level_right_db
         * @returns {Object} Formatted entry with human-readable values
         */
        formatLogEntry(entry) {
            const date = new Date(entry.timestamp);
            const isEnd = entry.event === 'silence_end';
            const isStart = entry.event === 'silence_start';
            const isTest = entry.event === 'test';

            // For ended events, show duration prominently in the event name
            let eventText = 'Unknown event';
            if (isEnd) {
                const dur = entry.duration_ms > 0 ? formatSmartDuration(entry.duration_ms) : '';
                eventText = dur ? `Silence ended: ${dur}` : 'Silence ended';
            } else if (isStart) {
                eventText = 'Silence detected';
            } else if (isTest) {
                eventText = 'Test entry';
            }

            // Map event type to state class
            let stateClass = 'state-stopped';
            if (isStart) stateClass = 'state-warning';
            else if (isEnd) stateClass = 'state-success';

            // Format audio levels if present
            const hasLevels = entry.level_left_db !== undefined && entry.level_right_db !== undefined;
            const levels = hasLevels
                ? `L ${entry.level_left_db.toFixed(1)} / R ${entry.level_right_db.toFixed(1)} dB`
                : '';

            return {
                time: date.toLocaleString(),
                event: eventText,
                stateClass,
                threshold: `${entry.threshold_db.toFixed(0)} dB`,
                levels
            };
        }
    }));
});
