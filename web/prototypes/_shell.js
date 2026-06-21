/*
 * Renders the shared settings tab strip (Audio | Notifications | Events |
 * About) so every prototype sits inside the same production-style chrome.
 * Call window.ProtoShell.tabs('events') after the DOM is ready.
 */
(function () {
    'use strict';

    var ICON = {
        audio: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M11 5 6 9H3v6h3l5 4z"/><path d="M15.5 8.5a5 5 0 0 1 0 7M19 5a9 9 0 0 1 0 14"/></svg>',
        notifications: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="5" width="18" height="14" rx="2"/><path d="m3 7 9 6 9-6"/></svg>',
        events: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01"/></svg>',
        about: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M12 11v5M12 8h.01"/></svg>'
    };

    var TABS = [
        { key: 'audio', label: 'Audio' },
        { key: 'notifications', label: 'Notifications' },
        { key: 'events', label: 'Events' },
        { key: 'about', label: 'About' }
    ];

    window.ProtoShell = {
        tabs: function (active, mountId) {
            var el = document.getElementById(mountId || 'tabs');
            if (!el) return;
            el.innerHTML = TABS.map(function (t) {
                return '<button class="shell-tab" aria-selected="' + (t.key === active) + '">' +
                    ICON[t.key] + '<span>' + t.label + '</span></button>';
            }).join('');
        }
    };
})();
