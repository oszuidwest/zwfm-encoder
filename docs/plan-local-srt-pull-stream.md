# Plan: lokale SRT pull-stream

> Status: **v1 geimplementeerd in code; target smoke-test nog uitvoeren op Pi/Linux build**.

## Kernbeslissing

Gebruik FFmpeg SRT `mode=listener`. Bouw geen eigen SRT-server en voeg geen
`srtgo` dependency toe.

V1-model:

- Een enabled listener-stream wordt altijd gestart.
- Clients tunen zelf in op de lokale SRT-poort.
- De app trackt geen individuele client-sessies.
- De app stopt een listener niet omdat er geen client is.
- De app stopt een listener alleen bij disable/delete, config update of encoder
  shutdown.
- Na client-disconnect start de app de FFmpeg listener opnieuw met
  listener-semantiek, niet via het caller retry/backoff-pad.

Waarom:

- FFmpeg ondersteunt SRT `mode=listener`.
- De bestaande streaming-laag draait al een eigen FFmpeg-proces per output.
- FFmpeg blijft verantwoordelijk voor encoding, muxing en SRT-output.
- FFmpeg's SRT listener accepteert in deze output-pipeline maar een client per
  FFmpeg-proces. Na disconnect is een nieuw FFmpeg listener-proces nodig.
- `srtgo` is een native libsrt binding en lost encoding/muxing niet op.

Bronnen:

- https://ffmpeg.org/ffmpeg-protocols.html#srt
- https://doc.haivision.com/SRT/1.5.3/Haivision/srt-connection-modes
- https://github.com/haivision/srtgo

## Doel

De encoder kan naast remote SRT push-streams ook een lokale SRT listener starten.
Een LAN-client kan verbinden met:

```text
srt://<encoder-ip>:9000?mode=caller
```

De Raspberry Pi blijft de audio-source; de client initieert alleen de SRT
verbinding.

## Niet in v1

- Geen eigen SRT-server in Go.
- Geen `srtgo`.
- Geen multi-client fan-out op een enkele SRT-poort.
- Geen per-client status, accept/reject of statistieken.
- Geen streamid-based routing/authenticatie.
- Geen nieuwe protocollen zoals HLS, Icecast, RTMP of RTP.
- Geen nieuw `stream_listening` event.
- Geen externe webhook/email/Zabbix alerts voor listener lifecycle.

## V1 contract

### Stream mode

Voeg mode toe aan `types.Stream`:

```go
type StreamMode string

const (
    StreamModeCaller   StreamMode = "caller"
    StreamModeListener StreamMode = "listener"
)

Mode StreamMode `json:"mode"`
```

Regels:

- Ontbrekende of lege mode blijft backwards-compatible `caller`.
- Nieuwe streams krijgen default `caller`, tenzij de gebruiker `listener` kiest.
- API responses sturen altijd `mode` terug.
- Gebruik een helper zoals `ModeOrDefault()`; legacy configs kunnen in memory
  `Mode == ""` houden.

### Config en validatie

Caller:

```json
{
  "mode": "caller",
  "host": "stream.example.com",
  "port": 9000,
  "stream_id": "studio",
  "codec": "pcm"
}
```

Listener:

```json
{
  "mode": "listener",
  "host": "",
  "port": 9000,
  "codec": "mp3"
}
```

Validatie:

- `caller`: host verplicht.
- `listener`: host optioneel, port verplicht.
- Lege listener-host wordt `0.0.0.0`.
- Expliciete listener-host is een bind-adres.
- Twee enabled listeners mogen niet hetzelfde genormaliseerde bind-adres en
  dezelfde poort gebruiken.
- Callers mogen dezelfde remote host/port blijven gebruiken.
- `stream_id` blijft caller-only in v1.

### SRT URL

Maak `BuildSRTURL` mode-aware.

Caller:

```text
srt://remote.example.com:9000?mode=caller&transtype=live&pkt_size=1316...
```

Listener:

```text
srt://0.0.0.0:9000?mode=listener&transtype=live&pkt_size=1316...
```

Listener URL-regels:

- Niet naar een remote host verbinden.
- Lege host expliciet als `0.0.0.0` renderen.
- Expliciete `listen_timeout=-1` zetten en op de target FFmpeg build valideren.
- Listener latency default `300000` us.
- Caller latency blijft ongewijzigd om productiegedrag niet te wijzigen.

Gewenst target-gedrag:

- Idle listener blijft wachten op een client.
- Client kan verbinden, disconnecten en later opnieuw verbinden doordat de app
  de listener opnieuw start.
- Geen readiness failure en geen extern alert door idle/disconnect.

### Password/encryptie

Huidige passphrase-logica is ambigu als `pbkeylen` ontbreekt. Maak dit voor alle
SRT streams expliciet:

- Geen password: geen `passphrase`, geen `pbkeylen`.
- Wel password: `passphrase=<password>` en `pbkeylen=16`.
- Valideer password-lengte op 10-64 tekens zodra een password is gezet.

### Codec

Nieuwe listener-streams krijgen `mp3` als default. Dat is de veiligste keuze voor
ad-hoc clients.

Andere codecs blijven beschikbaar:

- `pcm`: hoge kwaliteit voor libav/Liquidsoap/VLC/mpv, meer bandbreedte.
- `opus`: alleen gebruiken na client-test.

### Status en events

Zonder client-detectie is een running listener niet hetzelfde als een connected
push-stream.

V1 gedrag:

- Caller running/stable blijft `Connected`.
- Listener process running wordt `Listening`.
- Listener krijgt geen `stream_stable` event.
- Listener `ProcessStatus.Stable` blijft `false`.
- Gebruik het bestaande `stream_started` event met listener-tekst zoals
  `Listening on 0.0.0.0:9000`.
- Bij elke relisten-cyclus komt daardoor opnieuw een `stream_started` regel in
  de eventlog. Dat is bewust eventlog-only; stream lifecycle events lopen in de
  huidige architectuur niet naar webhook, email of Zabbix.
- Voeg geen nieuw `stream_listening` event toe in v1.

Echte `client_connected` status is latere scope. Dat vereist FFmpeg stderr
parsing, SRT stats of een andere relay/server-laag.

### Lifecycle

Basisregel: laat FFmpeg zelf listener zijn, maar behandel een afgeronde
listener-sessie anders dan een gefaalde caller-output.

- Start enabled listeners bij encoder start, net als caller streams.
- Stop listeners bij disable/delete, config update of shutdown.
- Doe niets actiefs op "geen client".
- Start opnieuw na client disconnect, omdat FFmpeg na een geaccepteerde
  listener-sessie niet teruggaat naar accept.

Broncheck:

- FFmpeg `libavformat/libsrt.c` roept in listener mode `srt_listen(fd, 1)` aan,
  wacht op readiness en doet daarna een enkele `srt_accept()`.
- Na accept sluit FFmpeg de listening socket en gebruikt het de accepted socket
  als output-socket.
- Bij disconnect faalt schrijven naar die socket; de muxer-write faalt en het
  FFmpeg-proces eindigt.
- `listen_timeout=-1` is de FFmpeg default en betekent: onbeperkt wachten op de
  eerste inkomende client. Zet dit expliciet in de listener URL.

V1 relisten-contract:

- Bind/start failure blijft een echte stream error.
- `ListenerStartFailureWindow` default `1s`.
- Exit binnen die window = bind/start failure, bestaand error/backoff-pad.
- Exit na die window = normale listener sessie-einde, geen `stream_error`, geen
  retry increment, geen exponentiele backoff.
- Restart na vaste `ListenerRelistenDelay` van 250-500 ms.

Geen restart-rate-cap in v1, tenzij de smoke-test een tight restart-loop
aantoont. Als die nodig is, bewaar de teller in de langlevende
`MonitorAndRetry` loop, niet op `streaming.Stream`.

Restrisico: een fout die consequent pas na `ListenerStartFailureWindow` optreedt,
kan als normale sessie-einde worden geclassificeerd en stil blijven relisten.
Log daarom minimaal een warning als relisten-frequentie abnormaal hoog wordt,
ook als een harde cap pas later komt.

### Readiness en health

- Enabled caller streams blijven meetellen in `/ready`.
- Enabled listener streams tellen niet mee in productie-readiness.
- Een idle/listening listener maakt `/ready` niet rood.
- Een idle/listening listener maakt `/ready` ook niet vals groen voor
  productie-output.
- Listener-fouten blijven zichtbaar in status, events en UI.
- `/health` blijft ongewijzigd.

### FFmpeg capability

Niet elke FFmpeg build heeft SRT. Deze workspace laat dat zien:

```bash
ffmpeg -hide_banner -h protocol=srt
# Unknown protocol 'srt'.
```

V1 moet SRT support detecteren:

```bash
ffmpeg -hide_banner -protocols
```

Acceptatie:

- `srt` moet aanwezig zijn.
- Alleen `srtp` is niet genoeg.
- SRT streams starten niet zonder SRT-capable FFmpeg.
- API/UI tonen een duidelijke capability-status of fout.

## Implementatiefases

### Fase 0 - target smoke-test

Voor of tijdens implementatie op de target Pi/Linux build:

1. Controleer `ffmpeg -hide_banner -protocols | grep -w srt`.
2. Start een losse FFmpeg SRT listener met de beoogde URL-opties.
3. Verifieer idle gedrag.
4. Verbind met `ffplay "srt://<encoder-ip>:9000?mode=caller"`.
5. Disconnect en bevestig dat het FFmpeg listener-proces eindigt.
6. Reconnect via de app en bevestig dat mode-aware relisten werkt.
7. Tune `ListenerStartFailureWindow` en `ListenerRelistenDelay` als de Pi-metingen
   daar aanleiding toe geven.

### Fase 1 - backend contract en URL

1. Voeg `StreamMode`, `ModeOrDefault()` en `mode` in API/config toe.
2. Maak streamvalidatie mode-aware.
3. Valideer duplicate enabled listener binds.
4. Maak `BuildSRTURL` mode-aware.
5. Voeg listener defaults toe:
   - bind host `0.0.0.0`;
   - latency `300000`;
   - expliciete `listen_timeout=-1`;
   - default codec `mp3` voor nieuwe listener-streams.
6. Maak password URL-opbouw expliciet met `pbkeylen=16`.
7. Valideer password-lengte op 10-64 tekens wanneer password gezet is.
8. Voeg FFmpeg SRT protocol-detectie toe.

### Fase 2 - lifecycle, status, readiness, UI

1. Start enabled listeners altijd.
2. Onderdruk `stream_stable` en `stable=true` voor listeners.
3. Toon running listener als `Listening`.
4. Sluit listeners uit van `/ready`.
5. Voeg mode-aware relisten-logica toe:
   - normale listener sessie-einde geeft geen `stream_error`;
   - normale listener sessie-einde verhoogt geen retry count;
   - normale listener sessie-einde gebruikt geen exponentiele backoff;
   - relisten gebruikt vaste delay.
6. Voeg UI mode-keuze toe:
   - `Push to server`;
   - `Local pull listener`.
7. Pas UI labels aan:
   - caller: `Host`, `Port`, `Stream ID`;
   - listener: `Bind address`, `Port`.
8. Verberg of disable `Stream ID` voor listener mode.
9. Toon copybare client URL:

```text
srt://<encoder-ip>:9000?mode=caller
```

### Fase 3 - docs en tests

1. Documenteer local SRT pull streams.
2. Documenteer codecadvies.
3. Documenteer UDP/firewall, FFmpeg SRT support en password/passphrase.
4. Voeg unit tests en handmatige smoke-test checklist toe.

## Tests

Unit tests:

- Stream mode defaulting en validatie.
- Duplicate listener bind validatie.
- `BuildSRTURL` voor caller en listener.
- Password URL-opbouw met en zonder password.
- Password-lengtevalidatie: leeg toegestaan, gezet password 10-64 tekens.
- Listener krijgt geen `stream_stable` en geen `stable=true`.
- `/ready` telt caller streams wel mee en listener streams niet.
- API redaction blijft password-safe en geeft `mode` terug.
- Listener relisten:
  - exit binnen `ListenerStartFailureWindow` gebruikt error/backoff;
  - exit na `ListenerStartFailureWindow` geeft geen `stream_error`, retry bump of
    exponentiele backoff;
  - normale relisten gebruikt vaste delay.

Handmatige smoke-test op target:

1. SRT support aanwezig.
2. Encoder start listener op gekozen poort.
3. UDP poort luistert.
4. `ffplay` kan verbinden.
5. Audio, codec en latency kloppen.
6. Disconnect sluit het FFmpeg listener-proces.
7. Reconnect via de app werkt na mode-aware relisten.
8. Idle/disconnect geeft geen false `stream_stable`, retry-bump, backoff of
   extern alert.
9. `/health` en `/ready` gedragen zich volgens contract.
10. Bind/start failure wordt wel zichtbaar als error.

## Risico's

### FFmpeg build zonder SRT

Niet elke FFmpeg heeft libsrt. Detecteer dit expliciet en geef een duidelijke
fout.

### Listener relisten-gap

Tussen disconnect, FFmpeg-exit, vaste relisten-delay en nieuwe FFmpeg startup is
de poort kort dicht. Een client die precies in dat venster reconnect, kan
connection refused krijgen. Voor monitoring is dat acceptabel; documenteer dit
als normaal gedrag.

### Listener zonder client

Een running listener is geen connected remote output. Toon daarom `Listening`,
niet `Connected`.

### Buffering bij idle listener

Als FFmpeg in listener mode niet uit stdin leest zolang er geen client is, kunnen
interne buffer drops ontstaan. Die mogen zichtbaar blijven, maar niet als
productie-readiness failure.

### Meerdere clients

V1 belooft geen multi-client fan-out. Voor meerdere luisteraars: meerdere
listener streams/poorten configureren of later een echte relay evalueren.

### Security

Een listener opent een UDP poort. Gebruik op minder vertrouwde LAN's een
passphrase. V1 heeft geen per-client access control.

## Latere scope

Alleen opnieuw ontwerpen als er behoefte is aan:

- meerdere simultane subscribers op een stream;
- per-client status/statistieken;
- streamid-based accept/reject;
- authenticatie/routing buiten FFmpeg;
- relay zonder extra FFmpeg-proces per listener.

Dan opnieuw vergelijken: `srtgo`, `datarhei/gosrt`, Liquidsoap, MediaMTX,
Restreamer of andere SRT tooling.

## Acceptatiecriteria

- Bestaande caller-streams blijven werken zonder configmigratie.
- Een nieuwe listener-stream kan via de UI worden aangemaakt.
- Enabled listener-streams worden altijd gestart.
- Clients kunnen via SRT intunen.
- Listener zonder client toont `Listening`, niet `Connected`.
- Listener emit geen vals `stream_stable`.
- Listener `ProcessStatus.Stable` blijft in v1 `false`.
- `/ready` baseert productie-readiness alleen op caller streams.
- Duplicate enabled listener binds worden afgewezen.
- Listener met lege host bindt op `0.0.0.0`.
- Listener URL gebruikt expliciet gevalideerde `listen_timeout=-1`.
- FFmpeg zonder SRT support geeft een duidelijke fout.
- Password-streams zetten `passphrase` en `pbkeylen=16`.
- Streams zonder password zetten geen `passphrase`/`pbkeylen`.
- Passwords van 1-9 of meer dan 64 tekens worden bij validatie geweigerd.
- Normale listener disconnect gebruikt relisten zonder `stream_error`, retry bump
  of exponentiele backoff.
- Tests dekken URL-bouw, validatie, API-redaction, readiness, status en relisten.
- Target smoke-test bevestigt SRT support, idle gedrag, disconnect-exit en
  reconnect via relisten.
