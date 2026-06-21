(function initEventHelpers(root) {
    const MS_PER_SECOND = 1000;
    const MS_PER_MINUTE = 60 * MS_PER_SECOND;
    // Keep in sync with internal/eventlog/logger.go MaxReadLimit.
    const EVENT_MAX_READ_LIMIT = 500;

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

    const nextEventWindowSize = (currentSize, pageSize) => Math.min(EVENT_MAX_READ_LIMIT, currentSize + pageSize);

    root.eventUIHelpers = {
        EVENT_MAX_READ_LIMIT,
        formatSmartDuration,
        nextEventWindowSize,
    };
})(globalThis);
