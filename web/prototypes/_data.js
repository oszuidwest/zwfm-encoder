/*
 * Shared sample data + formatting helpers for the events-page prototypes.
 *
 * EVENTS mirrors the real /api/events payload shape ({ ts, type, stream_id?,
 * details }) and is deliberately built to match what runs in production on
 * 172.18.1.11: a relentless hourly recorder cycle (recorder_file ->
 * upload_queued -> upload_completed, 3 near-identical rows per hour) with the
 * genuinely interesting events (stream errors, silence, imbalance, failed
 * uploads) sprinkled in. That ~95%-noise / 5%-signal ratio is the core problem
 * every prototype has to solve.
 *
 * EventUtil ports the label/severity/category/detail logic from web/app.js so
 * the prototypes stay faithful to production wording and colour mapping.
 */
(function () {
    'use strict';

    var MS_MIN = 60 * 1000;
    var MS_HOUR = 60 * MS_MIN;
    var MS_DAY = 24 * MS_HOUR;

    // ---- Faithful ports of web/app.js maps -----------------------------

    var LABELS = {
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
        cleanup_completed: 'Cleanup'
    };

    var SEVERITY = {
        stream_error: 'error',
        stream_retry: 'warning',
        stream_stable: 'success',
        silence_start: 'warning',
        silence_end: 'success',
        audio_dump_ready: 'info',
        channel_imbalance_start: 'warning',
        channel_imbalance_end: 'success',
        recorder_error: 'error',
        upload_failed: 'error',
        upload_abandoned: 'error',
        upload_completed: 'success',
        cleanup_completed: 'success',
        upload_retry: 'warning'
    };

    var CATEGORY_LABEL = {
        stream: 'Stream',
        audio: 'Audio',
        recorder: 'Recorder',
        system: 'System'
    };

    var STORAGE_LABELS = { both: 'Local + S3', s3: 'S3', local: 'Local' };

    var AUDIO_TYPES = {
        silence_start: 1, silence_end: 1, audio_dump_ready: 1,
        channel_imbalance_start: 1, channel_imbalance_end: 1
    };
    var RECORDER_TYPES = {
        recorder_started: 1, recorder_stopped: 1, recorder_error: 1,
        recorder_file: 1, upload_queued: 1, upload_completed: 1,
        upload_failed: 1, upload_retry: 1, upload_abandoned: 1,
        cleanup_completed: 1
    };

    // ROUTINE is the encoder's deterministic "everything is fine" heartbeat: the
    // four success-path types a healthy recorder emits every single hour. Source
    // of truth: internal/eventlog/logger.go (20 event types total) plus the
    // emission sites - recorder_file fires on the hourly rotationTimer
    // (recorder.go), upload_queued/upload_completed on each S3 push
    // (recorder_upload.go), cleanup_completed on hourly retention (cleanup.go).
    // With one recorder this is ~3-4 events/hour and was 481 of the last 500
    // live events. The encoder itself never raises an external alert for any of
    // them - it only alarms on silence and upload_abandoned (notify/notifier.go).
    // So these are noise by construction: log them, but they should not be the
    // default view. Everything NOT in this set is "notable" (a problem, a
    // recovery, or a deliberate lifecycle change) and is shown by default.
    var ROUTINE = {
        recorder_file: 1,
        upload_queued: 1,
        upload_completed: 1,
        cleanup_completed: 1
    };

    // Finer "why is this notable" classification, used by the signal-vs-noise
    // focused variants. PROBLEM = something is wrong or degraded (the encoder
    // would, for silence/upload_abandoned, even alert externally). RECOVERY =
    // the matching all-clear. Anything notable that is neither is a deliberate
    // LIFECYCLE change (start/stop, dump ready).
    var PROBLEM = {
        stream_error: 1, stream_retry: 1,
        recorder_error: 1, upload_failed: 1, upload_retry: 1, upload_abandoned: 1,
        silence_start: 1, channel_imbalance_start: 1
    };
    var RECOVERY = {
        stream_stable: 1, silence_end: 1, channel_imbalance_end: 1
    };

    // ---- Inline icons (16px, stroke = currentColor) --------------------

    var ICONS = {
        stream: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M5 12a7 7 0 0 1 7-7 7 7 0 0 1 7 7"/><path d="M8.5 12a3.5 3.5 0 0 1 7 0"/><circle cx="12" cy="12" r="1.5"/><path d="M12 13.5V21"/></svg>',
        audio: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12h3l3-8 4 16 3-8h5"/></svg>',
        recorder: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><circle cx="12" cy="12" r="3.5" fill="currentColor" stroke="none"/></svg>',
        system: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M12 8v4M12 16h.01"/></svg>',
        error: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M15 9l-6 6M9 9l6 6"/></svg>',
        warning: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0Z"/><path d="M12 9v4M12 17h.01"/></svg>',
        success: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M8.5 12.5l2.5 2.5 4.5-5"/></svg>',
        info: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M12 11v5M12 8h.01"/></svg>',
        upload: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 16V4M7 9l5-5 5 5"/><path d="M4 20h16"/></svg>',
        file: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/></svg>',
        silence: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M11 5 6 9H3v6h3l5 4z"/><path d="M22 9l-6 6M16 9l6 6"/></svg>',
        play: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 4l14 8-14 8z"/></svg>',
        stop: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="6" y="6" width="12" height="12" rx="2"/></svg>',
        clean: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18M8 6V4h8v2M6 6l1 14a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2l1-14"/></svg>'
    };

    // Per-type icon override; falls back to the category icon.
    var TYPE_ICON = {
        stream_stable: 'success',
        stream_error: 'error',
        stream_retry: 'warning',
        silence_start: 'silence',
        silence_end: 'success',
        audio_dump_ready: 'audio',
        channel_imbalance_start: 'warning',
        channel_imbalance_end: 'success',
        recorder_started: 'play',
        recorder_stopped: 'stop',
        recorder_error: 'error',
        recorder_file: 'file',
        upload_queued: 'upload',
        upload_completed: 'upload',
        upload_failed: 'error',
        upload_retry: 'warning',
        upload_abandoned: 'error',
        cleanup_completed: 'clean'
    };

    // ---- Helpers -------------------------------------------------------

    function isAudio(type) { return !!AUDIO_TYPES[type]; }
    function isRecorder(type) { return !!RECORDER_TYPES[type]; }

    // isRoutine accepts an event or a type string. Routine = the success-path
    // hourly heartbeat that should be hidden by default.
    function isRoutine(evOrType) {
        var t = typeof evOrType === 'string' ? evOrType : (evOrType && evOrType.type);
        return !!ROUTINE[t];
    }
    // tier: 'routine' (hide by default) or 'notable' (show by default).
    function tier(ev) { return isRoutine(ev) ? 'routine' : 'notable'; }
    // notable is the inverse - the events that actually tell a story.
    function isNotable(ev) { return !isRoutine(ev); }

    function typeOf(ev) { return typeof ev === 'string' ? ev : (ev && ev.type); }
    function isProblem(ev) { return !!PROBLEM[typeOf(ev)]; }
    function isRecovery(ev) { return !!RECOVERY[typeOf(ev)]; }
    function isLifecycle(ev) { return isNotable(ev) && !isProblem(ev) && !isRecovery(ev); }
    // reason: 'problem' | 'recovery' | 'lifecycle' | 'routine' - the single
    // word that explains why a row is (or is not) worth your attention.
    function reason(ev) {
        if (isRoutine(ev)) return 'routine';
        if (isProblem(ev)) return 'problem';
        if (isRecovery(ev)) return 'recovery';
        return 'lifecycle';
    }
    // subsystem: 'Streams' | 'Audio' | 'Recording' - for per-subsystem health.
    function subsystem(ev) {
        var c = category(ev);
        if (c === 'stream') return 'Streams';
        if (c === 'audio') return 'Audio';
        return 'Recording';
    }

    function category(ev) {
        if (isAudio(ev.type)) return 'audio';
        if (isRecorder(ev.type)) return 'recorder';
        if (ev.type && ev.type.indexOf('stream_') === 0) return 'stream';
        return 'system';
    }

    function formatDuration(ms) {
        if (ms < 1000) return Math.round(ms) + 'ms';
        if (ms < MS_MIN) {
            var sec = ms / 1000;
            return (sec < 10 ? sec.toFixed(1) : Math.round(sec)) + 's';
        }
        var mins = Math.floor(ms / MS_MIN);
        var secs = Math.round((ms % MS_MIN) / 1000);
        return secs > 0 ? mins + 'm ' + secs + 's' : mins + 'm';
    }

    function pad(n) { return n < 10 ? '0' + n : '' + n; }

    function time(ts) {
        var d = new Date(ts);
        return pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
    }
    function shortTime(ts) {
        var d = new Date(ts);
        return pad(d.getHours()) + ':' + pad(d.getMinutes());
    }

    function relativeTime(ts) {
        var diff = Date.now() - new Date(ts).getTime();
        if (diff < MS_MIN) return 'just now';
        if (diff < MS_HOUR) return Math.floor(diff / MS_MIN) + 'm ago';
        if (diff < MS_DAY) return Math.floor(diff / MS_HOUR) + 'h ago';
        return Math.floor(diff / MS_DAY) + 'd ago';
    }

    function dateKey(ts) {
        var d = new Date(ts);
        return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate());
    }

    function dateLabel(ts) {
        var date = new Date(ts);
        var now = new Date();
        var today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
        var day = new Date(date.getFullYear(), date.getMonth(), date.getDate());
        var diff = Math.round((today - day) / MS_DAY);
        if (diff === 0) return 'Today';
        if (diff === 1) return 'Yesterday';
        return date.toLocaleDateString('en-GB', { weekday: 'short', day: 'numeric', month: 'short' });
    }

    function severity(type) { return SEVERITY[type] || 'info'; }
    function label(type) { return LABELS[type] || type; }

    // Full (untruncated) source name for an event.
    function source(ev) {
        var d = ev.details || {};
        if (isAudio(ev.type)) return 'Audio';
        if (isRecorder(ev.type)) return d.recorder_name || 'Recorder';
        return d.stream_name || ev.stream_id || 'Stream';
    }

    // Ported from app.js getEventDetail().
    function detail(ev) {
        var d = ev.details || {};
        var t = ev.type;
        if (t === 'stream_error') return d.error || 'Unknown error';
        if (t === 'stream_retry') {
            var rn = d.retry ? 'Retry #' + d.retry : '';
            return [rn, d.error || ''].filter(Boolean).join(' - ');
        }
        if (t === 'silence_start') {
            if (d.level_left_db !== undefined) return 'L: ' + d.level_left_db.toFixed(1) + 'dB  R: ' + d.level_right_db.toFixed(1) + 'dB';
            return '';
        }
        if (t === 'silence_end') return d.duration_ms ? 'Duration: ' + formatDuration(d.duration_ms) : '';
        if (t === 'channel_imbalance_start') {
            if (d.imbalance_db !== undefined) return 'L: ' + d.level_left_db.toFixed(1) + 'dB  R: ' + d.level_right_db.toFixed(1) + 'dB (' + d.imbalance_db.toFixed(1) + 'dB diff)';
            return '';
        }
        if (t === 'channel_imbalance_end') return d.duration_ms ? 'Duration: ' + formatDuration(d.duration_ms) : '';
        if (t === 'audio_dump_ready') {
            if (d.dump_error) return 'Error: ' + d.dump_error;
            if (d.dump_filename) return d.dump_filename;
            return '';
        }
        if (t === 'recorder_error' || t === 'upload_failed') return d.error || 'Unknown error';
        if (t === 'recorder_file' || t === 'upload_queued' || t === 'upload_completed') {
            return [d.filename || '', (d.codec || '').toUpperCase()].filter(Boolean).join(' - ');
        }
        if (t === 'cleanup_completed') return (d.files_deleted || 0) + ' files deleted (' + (d.storage_type || '') + ')';
        if (t === 'recorder_started' || t === 'recorder_stopped') {
            var codec = d.codec ? d.codec.toUpperCase() : '';
            return [codec, STORAGE_LABELS[d.storage_mode] || ''].filter(Boolean).join(' - ');
        }
        return d.stream_name || '';
    }

    function iconFor(ev) {
        var key = TYPE_ICON[ev.type] || category(ev);
        return ICONS[key] || ICONS.system;
    }
    function categoryIcon(cat) { return ICONS[cat] || ICONS.system; }
    function severityIcon(sev) { return ICONS[sev] || ICONS.info; }

    // ---- Sample dataset ------------------------------------------------
    // Anchored to "now" so Today/Yesterday and relative times look live.

    function buildEvents() {
        var out = [];
        var now = Date.now();
        var topHour = new Date(now);
        topHour.setMinutes(0, 0, 0);
        var base = topHour.getTime();

        function push(offsetMs, type, details, stream_id) {
            var ev = { ts: new Date(base + offsetMs).toISOString(), type: type, details: details };
            if (stream_id) ev.stream_id = stream_id;
            out.push(ev);
        }

        function hourFile(h) { return 'zwfm-hourly-' + filestamp(base + h * MS_HOUR); }

        // 30 hours of the production hourly recorder cycle (the noise).
        for (var h = 0; h >= -30; h--) {
            var fname = hourFile(h) + '.mp3';
            var prev = hourFile(h - 1) + '.mp3';
            // New hour file created at :00.
            push(h * MS_HOUR + 90, 'recorder_file', { codec: 'mp3', filename: fname, recorder_name: 'zwfm-hourly', storage_mode: 's3' });
            // Previous hour queued + uploaded shortly after.
            push(h * MS_HOUR + 60, 'upload_queued', { codec: 'mp3', filename: prev, recorder_name: 'zwfm-hourly', storage_mode: 's3', s3_key: 'recordings/zwfm-hourly/' + prev });
            push(h * MS_HOUR + 95000 + (h % 3) * 20000, 'upload_completed', { codec: 'mp3', filename: prev, recorder_name: 'zwfm-hourly', storage_mode: 's3', s3_key: 'recordings/zwfm-hourly/' + prev });
        }

        // ---- Signal: the events that actually matter ----

        // A failed upload that retried then succeeded (8h ago).
        var f8 = hourFile(-8) + '.mp3';
        push(-8 * MS_HOUR + 30000, 'upload_failed', { codec: 'mp3', filename: f8, recorder_name: 'zwfm-hourly', storage_mode: 's3', s3_key: 'recordings/zwfm-hourly/' + f8, error: 'RequestTimeout: connection reset by peer', retry: 0 });
        push(-7 * MS_HOUR + 5000, 'upload_retry', { codec: 'mp3', filename: f8, recorder_name: 'zwfm-hourly', storage_mode: 's3', s3_key: 'recordings/zwfm-hourly/' + f8, retry: 1 });
        push(-7 * MS_HOUR + 22000, 'upload_completed', { codec: 'mp3', filename: f8, recorder_name: 'zwfm-hourly', storage_mode: 's3', s3_key: 'recordings/zwfm-hourly/' + f8 });

        // Backup stream dropped and reconnected (5h ago).
        push(-5 * MS_HOUR - 200000, 'stream_error', { stream_name: 'Backup (Studio B)', error: 'Error opening output files: Connection refused' }, 'stream-7be1');
        push(-5 * MS_HOUR - 197000, 'stream_retry', { stream_name: 'Backup (Studio B)', error: 'Connection refused', retry: 1, max_retries: 99 }, 'stream-7be1');
        push(-5 * MS_HOUR - 170000, 'stream_retry', { stream_name: 'Backup (Studio B)', error: 'Connection refused', retry: 2, max_retries: 99 }, 'stream-7be1');
        push(-5 * MS_HOUR - 150000, 'stream_stable', { stream_name: 'Backup (Studio B)' }, 'stream-7be1');

        // Silence + recovery + audio dump (4h ago).
        push(-4 * MS_HOUR, 'silence_start', { level_left_db: -52.4, level_right_db: -54.1, threshold_db: -40.0 });
        push(-4 * MS_HOUR + 78000, 'silence_end', { level_left_db: -14.2, level_right_db: -13.8, threshold_db: -40.0, duration_ms: 78000 });
        push(-4 * MS_HOUR + 81000, 'audio_dump_ready', { level_left_db: -14.2, level_right_db: -13.8, threshold_db: -40.0, duration_ms: 78000, dump_path: '/var/log/encoder/8080/dumps', dump_filename: 'silence-2026-06-21-080000.mp3', dump_size_bytes: 245760, dump_error: '' });

        // Channel imbalance flare (3h ago).
        push(-3 * MS_HOUR - 60000, 'channel_imbalance_start', { level_left_db: -6.2, level_right_db: -41.0, balance_db: 34.8, imbalance_db: 34.8, threshold_db: 12.0 });
        push(-3 * MS_HOUR - 42000, 'channel_imbalance_end', { level_left_db: -8.0, level_right_db: -9.5, balance_db: 1.5, imbalance_db: 1.5, threshold_db: 12.0, duration_ms: 18000 });

        // Hourly retention cleanup (2h ago).
        push(-2 * MS_HOUR + 200, 'cleanup_completed', { recorder_name: 'zwfm-hourly', files_deleted: 24, storage_type: 'local' });

        // An ad-hoc recorder started + stopped (90 / 20 min ago).
        push(-90 * MS_MIN, 'recorder_started', { recorder_name: 'Event Capture', codec: 'opus', storage_mode: 'local' });
        push(-20 * MS_MIN, 'recorder_stopped', { recorder_name: 'Event Capture', codec: 'opus', storage_mode: 'local' });

        // Main stream lifecycle near the very start of the window (newest in time order goes last).
        push(-29 * MS_HOUR, 'stream_started', { stream_name: 'Main (Transmitter)' }, 'stream-4ce8');
        push(-29 * MS_HOUR + 10000, 'stream_stable', { stream_name: 'Main (Transmitter)' }, 'stream-4ce8');

        // Newest-first, like the real API.
        out.sort(function (a, b) { return new Date(b.ts) - new Date(a.ts); });
        return out;
    }

    function filestamp(ms) {
        var d = new Date(ms);
        return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + '-' + pad(d.getHours()) + '-00';
    }

    // ---- Full event catalog -------------------------------------------
    // A faithful, source-grounded set that exercises EVERY one of the 20
    // event types the encoder emits (internal/eventlog/logger.go), with the
    // exact detail fields from the *Details structs and the msg strings from
    // the emission sites - and, crucially, the UNRESOLVED paths the main sample
    // omits: an ongoing silence and imbalance, a stream still retrying, an
    // upload abandoned after 24h, and a recorder error that has not cleared.
    // Citations point at the emitter so the data stays honest.
    function buildCatalog() {
        var out = [];
        var now = Date.now();
        var topHour = new Date(now);
        topHour.setMinutes(0, 0, 0);
        var base = topHour.getTime();

        // Recent / signal / activity events anchor to "now" so a "-8 min" event
        // really reads as 8 minutes ago. (Anchoring to the top of the hour would
        // skew minute-scale events by up to 59 minutes.)
        function push(offsetMs, type, details, extra) {
            var ev = { ts: new Date(now + offsetMs).toISOString(), type: type, details: details || {} };
            if (extra && extra.stream_id) ev.stream_id = extra.stream_id;
            if (extra && extra.msg) ev.msg = extra.msg;
            out.push(ev);
        }
        // The hourly heartbeat aligns to real clock-hour boundaries instead.
        function pushHour(absMs, type, details) {
            out.push({ ts: new Date(absMs).toISOString(), type: type, details: details || {} });
        }
        function catFile(h) { return 'zwfm-hourly-' + filestamp(base + h * MS_HOUR) + '.mp3'; }

        // === UNRESOLVED - still happening, no recovery seen ===

        // Ongoing channel imbalance: right channel near-dead, no _end yet.
        // ImbalanceDetails: level_left/right_db, balance_db (signed L-R),
        // imbalance_db (abs), threshold_db. (notify/notifier.go logChannelImbalanceStart)
        push(-12 * MS_MIN, 'channel_imbalance_start', { level_left_db: -5.8, level_right_db: -47.9, balance_db: 42.1, imbalance_db: 42.1, threshold_db: 12.0 });

        // Ongoing silence: input dead 8 minutes, no silence_end yet.
        // SilenceDetails: level_left/right_db, threshold_db. (notify/notifier.go logSilenceStart)
        push(-8 * MS_MIN, 'silence_start', { level_left_db: -58.2, level_right_db: -57.6, threshold_db: -40.0 });

        // Main transmitter dropped and is STILL retrying (no stream_stable).
        // stream_error msg "Stream failed"; stream_retry msg "Retrying in 6s",
        // retry/max_retries=99. (streaming/manager.go:919,992; DefaultMaxRetries=99)
        push(-6 * MS_MIN, 'stream_error', { stream_name: 'Main (Transmitter)', error: 'Connection timed out' }, { stream_id: 'stream-4ce8', msg: 'Stream failed' });
        push(-5 * MS_MIN, 'stream_retry', { stream_name: 'Main (Transmitter)', retry: 1, max_retries: 99 }, { stream_id: 'stream-4ce8', msg: 'Retrying in 3s' });
        push(-4 * MS_MIN, 'stream_retry', { stream_name: 'Main (Transmitter)', retry: 2, max_retries: 99 }, { stream_id: 'stream-4ce8', msg: 'Retrying in 6s' });
        push(-2 * MS_MIN, 'stream_retry', { stream_name: 'Main (Transmitter)', retry: 3, max_retries: 99 }, { stream_id: 'stream-4ce8', msg: 'Retrying in 12s' });

        // Recorder error that has not cleared: local spool path not writable.
        // (recording/recorder.go:275 RecorderError; example error from docs/events.md)
        push(-32 * MS_MIN, 'recorder_error', { recorder_name: 'Archive', codec: 'mp3', storage_mode: 'local', error: 'local path is not writable' });

        // Upload abandoned after 24h of retries (terminal failure, alerts fire).
        // RecorderDetails: error(last), retry(24). (recorder_upload.go:336; MaxUploadRetryAge=24h)
        var aband = 'zwfm-hourly-' + filestamp(base - 25 * MS_HOUR) + '.mp3';
        push(-46 * MS_MIN, 'upload_abandoned', { recorder_name: 'zwfm-hourly', filename: aband, codec: 'mp3', storage_mode: 's3', s3_key: 'recordings/zwfm-hourly/' + aband, error: 'RequestTimeout: request timed out', retry: 24 });

        // === RESOLVED - problem followed by its recovery ===

        // Silence episode that recovered, with the MP3 dump that follows.
        push(-3 * MS_HOUR, 'silence_start', { level_left_db: -52.4, level_right_db: -54.1, threshold_db: -40.0 });
        push(-3 * MS_HOUR + 78000, 'silence_end', { level_left_db: -14.2, level_right_db: -13.8, threshold_db: -40.0, duration_ms: 78000 });
        push(-3 * MS_HOUR + 81000, 'audio_dump_ready', { level_left_db: -14.2, level_right_db: -13.8, threshold_db: -40.0, duration_ms: 78000, dump_path: '/var/log/encoder/8080/dumps', dump_filename: 'silence-' + filestamp(base - 3 * MS_HOUR) + '.mp3', dump_size_bytes: 245760, dump_error: '' });

        // Backup stream outage that reconnected.
        push(-5 * MS_HOUR - 200000, 'stream_error', { stream_name: 'Backup (Studio B)', error: 'Error opening output files: Connection refused' }, { stream_id: 'stream-7be1', msg: 'Stream failed' });
        push(-5 * MS_HOUR - 188000, 'stream_retry', { stream_name: 'Backup (Studio B)', retry: 1, max_retries: 99 }, { stream_id: 'stream-7be1', msg: 'Retrying in 3s' });
        push(-5 * MS_HOUR - 170000, 'stream_retry', { stream_name: 'Backup (Studio B)', retry: 2, max_retries: 99 }, { stream_id: 'stream-7be1', msg: 'Retrying in 6s' });
        push(-5 * MS_HOUR - 150000, 'stream_stable', { stream_name: 'Backup (Studio B)' }, { stream_id: 'stream-7be1', msg: 'Stream connected and stable' });

        // Channel imbalance that recovered.
        push(-7 * MS_HOUR - 18000, 'channel_imbalance_start', { level_left_db: -6.2, level_right_db: -41.0, balance_db: 34.8, imbalance_db: 34.8, threshold_db: 12.0 });
        push(-7 * MS_HOUR, 'channel_imbalance_end', { level_left_db: -8.0, level_right_db: -9.5, balance_db: 1.5, imbalance_db: 1.5, threshold_db: 12.0, duration_ms: 18000 });

        // Upload that failed, retried, then completed.
        var f9 = catFile(-9);
        push(-9 * MS_HOUR + 30000, 'upload_failed', { recorder_name: 'zwfm-hourly', filename: f9, codec: 'mp3', storage_mode: 's3', s3_key: 'recordings/zwfm-hourly/' + f9, error: 'RequestTimeout: connection reset by peer', retry: 0 });
        push(-8 * MS_HOUR + 5000, 'upload_retry', { recorder_name: 'zwfm-hourly', filename: f9, codec: 'mp3', storage_mode: 's3', s3_key: 'recordings/zwfm-hourly/' + f9, retry: 1 });
        push(-8 * MS_HOUR + 22000, 'upload_completed', { recorder_name: 'zwfm-hourly', filename: f9, codec: 'mp3', storage_mode: 's3', s3_key: 'recordings/zwfm-hourly/' + f9, retry: 1 });

        // === ACTIVITY - deliberate lifecycle changes ===

        // Caller stream brought up: "Connecting to ..." then stable. (manager.go:283,150)
        push(-30 * MS_HOUR, 'stream_started', { stream_name: 'Main (Transmitter)' }, { stream_id: 'stream-4ce8', msg: 'Connecting to 195.201.170.190:8888' });
        push(-30 * MS_HOUR + 10000, 'stream_stable', { stream_name: 'Main (Transmitter)' }, { stream_id: 'stream-4ce8', msg: 'Stream connected and stable' });
        // Listener fan-out: "Listening on ..." (no stable for listeners). (manager.go:369)
        push(-28 * MS_HOUR, 'stream_started', { stream_name: 'Pull listener' }, { stream_id: 'stream-9ab2', msg: 'Listening on srt://0.0.0.0:9999' });
        // A relay stopped by the user. (manager.go:569)
        push(-20 * MS_HOUR, 'stream_stopped', { stream_name: 'Old Relay' }, { stream_id: 'stream-3c10', msg: 'Stream stopped by user' });
        // Hourly recorder started at boot. (recorder.go:262)
        push(-30 * MS_HOUR - 5000, 'recorder_started', { recorder_name: 'zwfm-hourly', codec: 'mp3', storage_mode: 'both' });
        // Ad-hoc capture started then stopped. (recorder.go:262,347)
        push(-3 * MS_HOUR - 10 * MS_MIN, 'recorder_started', { recorder_name: 'Event Capture', codec: 'opus', storage_mode: 'local' });
        push(-95 * MS_MIN, 'recorder_stopped', { recorder_name: 'Event Capture', codec: 'opus', storage_mode: 'local' });

        // === ROUTINE - the hourly heartbeat (recorder_file/upload_queued/
        // upload_completed) plus periodic retention cleanup. ===
        for (var h = 0; h >= -12; h--) {
            var hourBase = base + h * MS_HOUR;
            // Only emit hours that have actually elapsed (the :00 file for the
            // current hour exists; its upload completes ~90s later).
            if (hourBase + 92000 > now) continue;
            var fname = catFile(h);
            var prev = catFile(h - 1);
            pushHour(hourBase + 90, 'recorder_file', { recorder_name: 'zwfm-hourly', filename: fname, codec: 'mp3', storage_mode: 'both' });
            // Skip the clean upload for the two files whose uploads are their own
            // incidents above (the one that failed-then-recovered, and the
            // abandoned one) so they are not double-counted.
            if (prev !== f9 && prev !== aband) {
                pushHour(hourBase + 60, 'upload_queued', { recorder_name: 'zwfm-hourly', filename: prev, codec: 'mp3', storage_mode: 'both', s3_key: 'recordings/zwfm-hourly/' + prev });
                pushHour(hourBase + 92000, 'upload_completed', { recorder_name: 'zwfm-hourly', filename: prev, codec: 'mp3', storage_mode: 'both', s3_key: 'recordings/zwfm-hourly/' + prev });
            }
            if (h % 4 === 0) pushHour(hourBase + 200, 'cleanup_completed', { recorder_name: 'zwfm-hourly', files_deleted: 24, storage_type: 'local' });
        }

        out.sort(function (a, b) { return new Date(b.ts) - new Date(a.ts); });
        return out;
    }

    var EVENTS = buildEvents();
    var CATALOG = buildCatalog();

    window.EVENTS = EVENTS;
    window.CATALOG = CATALOG;
    window.EventUtil = {
        LABELS: LABELS,
        SEVERITY: SEVERITY,
        CATEGORY_LABEL: CATEGORY_LABEL,
        ICONS: ICONS,
        label: label,
        severity: severity,
        category: category,
        categoryLabel: function (cat) { return CATEGORY_LABEL[cat] || 'System'; },
        source: source,
        detail: detail,
        isAudio: isAudio,
        isRecorder: isRecorder,
        isRoutine: isRoutine,
        isNotable: isNotable,
        tier: tier,
        isProblem: isProblem,
        isRecovery: isRecovery,
        isLifecycle: isLifecycle,
        reason: reason,
        subsystem: subsystem,
        ROUTINE: ROUTINE,
        PROBLEM: PROBLEM,
        RECOVERY: RECOVERY,
        icon: iconFor,
        categoryIcon: categoryIcon,
        severityIcon: severityIcon,
        time: time,
        shortTime: shortTime,
        relativeTime: relativeTime,
        dateKey: dateKey,
        dateLabel: dateLabel,
        formatDuration: formatDuration
    };
})();
