/**
 * ZuidWest FM Encoder - Alpine.js Web Application
 *
 * Real-time audio monitoring, encoder control, and multi-output stream
 * management via WebSocket connection to Go backend.
 *
 * Architecture:
 *   - Single Alpine.js component (encoderApp) manages all UI state
 *   - WebSocket connection at /ws for bidirectional communication
 *   - Three views: dashboard (monitoring), settings (config), add-output (form)
 *
 * WebSocket Message Types (incoming):
 *   - levels: Audio RMS/peak levels, ~4 updates per second
 *   - status: Encoder state, outputs, devices, settings (every 3s)
 *   - test_result: Unified notification test result with test_type field
 *
 * WebSocket Commands (outgoing):
 *   - start/stop: Control encoder
 *   - update_settings: Persist configuration changes
 *   - add_output/delete_output: Manage stream outputs
 *   - test_<type>: Trigger notification test (webhook, log, email)
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
const EMAIL_FEEDBACK_MS = 2000;   // Email test result display duration

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
    streamid: '',
    password: '',
    codec: 'wav',
    max_retries: 99
};

const DEFAULT_EDIT_OUTPUT = {
    ...DEFAULT_OUTPUT,
    id: '',
    enabled: true,
    created_at: 0
};

const DEFAULT_LEVELS = {
    left: -60,
    right: -60,
    peak_left: -60,
    peak_right: -60,
    silence_level: null
};

// Settings field mapping for WebSocket status sync
const SETTINGS_MAP = [
    { msgKey: 'silence_threshold', path: 'silenceThreshold', default: -40 },
    { msgKey: 'silence_duration', path: 'silenceDuration', default: 15 },
    { msgKey: 'silence_recovery', path: 'silenceRecovery', default: 5 },
    { msgKey: 'silence_webhook', path: 'silenceWebhook', default: '' },
    { msgKey: 'silence_log_path', path: 'silenceLogPath', default: '' },
    { msgKey: 'email_smtp_host', path: 'email.host', default: '' },
    { msgKey: 'email_smtp_port', path: 'email.port', default: 587 },
    { msgKey: 'email_from_name', path: 'email.fromName', default: 'ZuidWest FM Encoder' },
    { msgKey: 'email_username', path: 'email.username', default: '' },
    { msgKey: 'email_recipients', path: 'email.recipients', default: '' }
];

/**
 * Sets a nested property value using dot-notation path.
 *
 * @param {Object} obj - Target object to modify
 * @param {string} path - Dot-notation path (e.g., 'email.host')
 * @param {*} value - Value to set
 */
function setNestedValue(obj, path, value) {
    const keys = path.split('.');
    let current = obj;
    for (let i = 0; i < keys.length - 1; i++) {
        if (!Object.hasOwn(current, keys[i])) {
            console.warn(`setNestedValue: invalid path '${path}' - key '${keys[i]}' not found`);
            return;
        }
        current = current[keys[i]];
    }
    const finalKey = keys[keys.length - 1];
    if (!Object.hasOwn(current, finalKey)) {
        console.warn(`setNestedValue: invalid path '${path}' - final key '${finalKey}' not found`);
        return;
    }
    current[finalKey] = value;
}

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
            { id: 'notifications', label: 'Notifications', icon: 'bell' },
            { id: 'about', label: 'About', icon: 'info' }
        ],

        newOutput: { ...DEFAULT_OUTPUT },

        editOutput: { ...DEFAULT_EDIT_OUTPUT },
        editOutputDirty: false,

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
            email: { host: '', port: 587, fromName: 'ZuidWest FM Encoder', username: '', password: '', recipients: '' },
            platform: ''
        },
        originalSettings: null,
        settingsDirty: false,

        version: { current: '', latest: '', updateAvail: false, commit: '', build_time: '' },

        // Notification test state (unified object for all test types)
        testStates: {
            webhook: { pending: false, text: 'Test' },
            log: { pending: false, text: 'Test' },
            email: { pending: false, text: 'Test' }
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

        // Lifecycle
        /**
         * Alpine.js lifecycle hook - initializes WebSocket connection.
         * Called automatically when component mounts.
         */
        init() {
            this.connectWebSocket();
            // Global keyboard handlers
            document.addEventListener('keydown', (e) => this.handleGlobalKeydown(e));
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
                    this.closeSilenceLog();
                    event.preventDefault();
                } else if (this.view === 'settings') {
                    this.cancelSettings();
                    event.preventDefault();
                } else if (this.view === 'add-output') {
                    this.showDashboard();
                    event.preventDefault();
                } else if (this.view === 'edit-output') {
                    this.cancelEditOutput();
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
         * Navigates to adjacent tab in settings.
         * @param {number} direction - 1 for next, -1 for previous
         */
        navigateTab(direction) {
            const currentIndex = this.settingsTabs.findIndex(t => t.id === this.settingsTab);
            const newIndex = (currentIndex + direction + this.settingsTabs.length) % this.settingsTabs.length;
            this.showTab(this.settingsTabs[newIndex].id);
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
                const msg = JSON.parse(e.data);
                if (msg.type === 'levels') {
                    this.handleLevels(msg.levels);
                } else if (msg.type === 'status') {
                    this.handleStatus(msg);
                } else if (msg.type === 'test_result') {
                    this.handleTestResult(msg);
                } else if (msg.type === 'silence_log_result') {
                    this.handleSilenceLogResult(msg);
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
         * @param {string} type - Command type (start, stop, update_settings, etc.)
         * @param {string} [id] - Optional output ID for output-specific commands
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
            const prevSilenceClass = this.getSilenceClass();
            this.levels = levels;
            const newSilenceClass = this.getSilenceClass();

            if (newSilenceClass !== prevSilenceClass) {
                this.handleSilenceTransition(prevSilenceClass, newSilenceClass);
            }

            // Update banner message with current duration if silence banner is showing
            if (this.banner.visible && this.banner.type !== 'info' && levels.silence_duration) {
                const duration = this.formatDuration(levels.silence_duration);
                if (newSilenceClass === 'critical') {
                    this.banner.message = `Critical silence: ${duration}`;
                } else if (newSilenceClass === 'warning') {
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
         * @param {string} prev - Previous silence class
         * @param {string} next - New silence class
         */
        handleSilenceTransition(prev, next) {
            const duration = this.formatDuration(this.levels.silence_duration || 0);
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
         * Returns escalating CSS class based on silence duration.
         * Thresholds are based on configured silenceDuration:
         * - --silence-active: silenceDuration (alert triggered)
         * - --silence-warning: silenceDuration * 2
         * - --silence-critical: silenceDuration * 4
         * @returns {string} BEM modifier class: '' | 'vu__indicator-dot--silence-active' | etc.
         */
        getSilenceClass() {
            if (!this.levels.silence_level) return '';
            const duration = this.levels.silence_duration || 0;
            const threshold = this.settings.silenceDuration || 15;
            if (duration >= threshold * 4) return 'vu__indicator-dot--silence-critical';
            if (duration >= threshold * 2) return 'vu__indicator-dot--silence-warning';
            return 'vu__indicator-dot--silence-active';
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
                    // Trigger animation by temporarily adding class
                    this.$nextTick(() => {
                        const dotElement = document.querySelector(`[data-output-id="${id}"] .output__dot`);
                        if (dotElement) {
                            dotElement.classList.add('output__dot--just-connected');
                            setTimeout(() => {
                                dotElement.classList.remove('output__dot--just-connected');
                            }, 400);
                        }
                    });
                }
            }

            this.previousOutputStatuses = structuredClone(newOutputStatuses);
            this.outputStatuses = newOutputStatuses;

            for (const id in this.deletingOutputs) {
                const output = this.outputs.find(o => o.id === id);
                if (!output || output.created_at !== this.deletingOutputs[id]) {
                    delete this.deletingOutputs[id];
                }
            }

            // Devices
            if (msg.devices) {
                this.devices = msg.devices;
            }

            // Only update settings from status when not on settings view to prevent
            // overwriting user input while editing
            if (this.view !== 'settings') {
                if (msg.settings?.audio_input) {
                    this.settings.audioInput = msg.settings.audio_input;
                }
                // Sync remaining settings from status message
                for (const field of SETTINGS_MAP) {
                    if (msg[field.msgKey] !== undefined) {
                        setNestedValue(this.settings, field.path, msg[field.msgKey] || field.default);
                    }
                }
                if (msg.settings?.platform !== undefined) {
                    this.settings.platform = msg.settings.platform;
                }
            }

            if (msg.version) {
                const wasUpdateAvail = this.version.updateAvail;
                this.version = msg.version;
                // Show banner once when update becomes available
                if (msg.version.updateAvail && !wasUpdateAvail) {
                    this.showBanner(`Update available: ${msg.version.latest}`, 'info', false);
                }
            }
        },

        /**
         * Handles notification test result from backend.
         * Updates UI feedback and auto-clears after EMAIL_FEEDBACK_MS.
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
            setTimeout(() => { this.testStates[type].text = 'Test'; }, EMAIL_FEEDBACK_MS);
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
                'add-output': 'add-output-view',
                'edit-output': 'edit-output-view'
            };
            const viewId = viewIds[this.view] || 'settings-view';
            const saveBtn = document.querySelector(`#${viewId} .nav-btn--save`);
            if (saveBtn) {
                saveBtn.classList.add('nav-btn--saved');
                setTimeout(() => {
                    saveBtn.classList.remove('nav-btn--saved');
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
            this.originalSettings = structuredClone(this.settings);
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
         * Reverts settings to snapshot taken when entering settings view.
         * Returns to dashboard without saving changes.
         */
        cancelSettings() {
            if (this.originalSettings) {
                this.settings = structuredClone(this.originalSettings);
            }
            this.showDashboard();
        },

        /**
         * Persists all settings to backend via WebSocket.
         * Builds payload with all current values, only including password
         * if it was modified (non-empty). Resets dirty state on send.
         */
        saveSettings() {
            const update = {
                silence_threshold: this.settings.silenceThreshold,
                silence_duration: this.settings.silenceDuration,
                silence_recovery: this.settings.silenceRecovery,
                silence_webhook: this.settings.silenceWebhook,
                silence_log_path: this.settings.silenceLogPath,
                email_smtp_host: this.settings.email.host,
                email_smtp_port: this.settings.email.port,
                email_from_name: this.settings.email.fromName,
                email_username: this.settings.email.username,
                email_recipients: this.settings.email.recipients
            };
            // Only include password if it was changed
            if (this.settings.email.password) {
                update.email_password = this.settings.email.password;
            }
            this.send('update_settings', null, update);
            this.saveAndClose();
        },

        showAddOutput() {
            this.newOutput = { ...DEFAULT_OUTPUT };
            this.view = 'add-output';
        },

        /**
         * Switches active settings tab.
         * @param {string} tabId - Tab identifier (audio, notifications, about)
         */
        showTab(tabId) {
            this.settingsTab = tabId;
        },

        // Output management
        /**
         * Validates and submits new output configuration.
         * Requires host and port; other fields have defaults.
         */
        submitNewOutput() {
            if (!this.newOutput.host) {
                return;
            }
            this.send('add_output', null, {
                host: this.newOutput.host.trim(),
                port: this.newOutput.port,
                streamid: this.newOutput.streamid.trim() || 'studio',
                password: this.newOutput.password,
                codec: this.newOutput.codec,
                max_retries: this.newOutput.max_retries
            });
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
            this.send('delete_output', id, null);
            if (returnToDashboard) this.showDashboard();
        },

        /**
         * Opens the edit output view and populates it with existing output data.
         * @param {string} id - ID of the output to edit
         */
        showEditOutput(id) {
            const output = this.outputs.find(o => o.id === id);
            if (!output) return;

            this.editOutput = {
                id: output.id,
                host: output.host,
                port: output.port,
                streamid: output.streamid || '',
                password: '',
                codec: output.codec || 'wav',
                max_retries: output.max_retries || 99,
                enabled: output.enabled !== false,
                created_at: output.created_at
            };
            this.editOutputDirty = false;
            this.view = 'edit-output';
        },

        /**
         * Marks edit output form as dirty (modified).
         */
        markEditOutputDirty() {
            this.editOutputDirty = true;
        },

        /**
         * Cancels edit and returns to dashboard without saving.
         */
        cancelEditOutput() {
            this.editOutput = { ...DEFAULT_EDIT_OUTPUT };
            this.editOutputDirty = false;
            this.showDashboard();
        },

        /**
         * Submits the edited output to the backend.
         */
        submitEditOutput() {
            if (!this.editOutput.host || !this.editOutput.id) return;

            const data = {
                host: this.editOutput.host.trim(),
                port: this.editOutput.port,
                streamid: this.editOutput.streamid.trim() || 'studio',
                codec: this.editOutput.codec,
                max_retries: this.editOutput.max_retries,
                enabled: this.editOutput.enabled,
                created_at: this.editOutput.created_at
            };

            if (this.editOutput.password) {
                data.password = this.editOutput.password;
            }

            this.send('update_output', this.editOutput.id, data);
            this.saveAndClose();
        },

        /**
         * Deletes the output being edited and returns to dashboard.
         */
        confirmDeleteOutput() {
            this.deleteOutput(this.editOutput.id, true);
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

            // Handle disabled outputs first (explicit from backend)
            if (status.disabled) {
                return {
                    stateClass: 'state-stopped',
                    statusText: 'Disabled',
                    showError: false,
                    lastError: ''
                };
            }

            // Compute state class
            let stateClass;
            if (isDeleting) stateClass = 'state-warning';
            else if (status.stable) stateClass = 'state-success';
            else if (status.given_up) stateClass = 'state-danger';
            else if (status.retry_count > 0) stateClass = 'state-warning';
            else if (status.running) stateClass = 'state-warning';
            else if (!this.encoderRunning) stateClass = 'state-stopped';
            else stateClass = 'state-warning';

            // Compute status text
            let statusText;
            if (isDeleting) statusText = 'Deleting...';
            else if (status.stable) statusText = 'Connected';
            else if (status.given_up) statusText = 'Failed';
            else if (status.retry_count > 0) statusText = `Retry ${status.retry_count}/${status.max_retries}`;
            else if (status.running) statusText = 'Connecting...';
            else if (!this.encoderRunning) statusText = 'Offline';
            else statusText = 'Connecting...';

            // Compute error visibility
            const showError = !isDeleting && (status.given_up || status.retry_count > 0) && status.last_error;

            return {
                stateClass,
                statusText,
                showError,
                lastError: status.last_error || ''
            };
        },

        /**
         * Gets output status and deletion state.
         * @deprecated Use getOutputDisplayData() for better performance
         *
         * @param {Object} output - Output object with id and created_at
         * @returns {Object} Object with status and isDeleting properties
         */
        getOutputStatus(output) {
            return {
                status: this.outputStatuses[output.id] || {},
                isDeleting: this.deletingOutputs[output.id] === output.created_at
            };
        },

        /**
         * Determines CSS state class for output status indicator.
         * Priority: deleting > encoder stopped > failed > retrying > connected.
         * @deprecated Use getOutputDisplayData().stateClass for better performance
         *
         * @param {Object} output - Output configuration object
         * @returns {string} CSS class for state styling
         */
        getOutputStateClass(output) {
            return this.getOutputDisplayData(output).stateClass;
        },

        /**
         * Generates human-readable status text for output.
         * @deprecated Use getOutputDisplayData().statusText for better performance
         *
         * @param {Object} output - Output configuration object
         * @returns {string} Status text (e.g., 'Connected', 'Retry 2/5')
         */
        getOutputStatusText(output) {
            return this.getOutputDisplayData(output).statusText;
        },

        /**
         * Determines if error message should be shown for output.
         * Shows error when output has failed state with error message.
         * @deprecated Use getOutputDisplayData().showError for better performance
         *
         * @param {Object} output - Output configuration object
         * @returns {boolean} True if error should be displayed
         */
        shouldShowError(output) {
            return this.getOutputDisplayData(output).showError;
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
            this.send(`test_${type}`, null, null);
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
            this.send('view_silence_log', null, null);
        },

        closeSilenceLog() {
            this.silenceLogModal.visible = false;
        },

        refreshSilenceLog() {
            this.silenceLogModal.loading = true;
            this.send('view_silence_log', null, null);
        },

        /**
         * Shows an alert banner notification.
         * @param {string} message - Message to display
         * @param {string} type - Banner type: 'info', 'warning', 'danger'
         * @param {boolean} persistent - If true, banner stays until dismissed
         */
        showBanner(message, type = 'info', persistent = false) {
            this.banner = { visible: true, message, type, persistent };
            if (!persistent) {
                setTimeout(() => this.hideBanner(), 10000);
            }
        },

        hideBanner() {
            this.banner.visible = false;
        },

        /**
         * Formats duration in human-readable format.
         * @param {number} seconds - Duration in seconds
         * @returns {string} Formatted duration (e.g., "1m 6s" or "45s")
         */
        formatDuration(seconds) {
            if (seconds < 60) return `${Math.round(seconds)}s`;
            const mins = Math.floor(seconds / 60);
            const secs = Math.round(seconds % 60);
            return secs > 0 ? `${mins}m ${secs}s` : `${mins}m`;
        },

        /**
         * Formats a silence log entry for display.
         * For "ended" events, duration is the key metric (total silence time).
         * For "started" events, duration is just detection delay (not shown).
         * @param {Object} entry - Log entry with timestamp, event, duration_sec, threshold_db
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
                const dur = entry.duration_sec > 0 ? this.formatDuration(entry.duration_sec) : '';
                eventText = dur ? `Silence ended: ${dur}` : 'Silence ended';
            } else if (isStart) {
                eventText = 'Silence detected';
            } else if (isTest) {
                eventText = 'Test entry';
            }

            return {
                time: date.toLocaleString(),
                event: eventText,
                eventClass: isStart ? 'log__entry--silence' : isEnd ? 'log__entry--recovery' : 'log__entry--test',
                // Only show threshold, duration is now in the event name for ended events
                threshold: `${entry.threshold_db.toFixed(0)} dB`
            };
        }
    }));
});
