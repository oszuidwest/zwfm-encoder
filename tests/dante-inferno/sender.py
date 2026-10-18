import os
import signal
import subprocess
import time

children = []
logs = []
def start(args, name, **kwargs):
    log = open('/test/' + name + '.log', 'ab', buffering=0)
    logs.append(log)
    p = subprocess.Popen(args, stderr=log, stdout=log, **kwargs)
    children.append(p)
    return p

signal.signal(signal.SIGTERM, lambda *_: exit(0))
try:
    env = dict(os.environ, USRVCLOCK_SOCKET='/tmp/audit-clock',
               INFERNO_CLOCK_PATH='/tmp/audit-clock', INFERNO_NAME='inferno-audit',
               INFERNO_BIND_IP='eth0', INFERNO_SAMPLE_RATE='48000',
               INFERNO_TX_CHANNELS='2', INFERNO_RX_CHANNELS='2',
               INFERNO_ALT_PORT='14450', INFERNO_PROCESS_ID='1', RUST_LOG='info')
    start(['test-clock'], 'clock', env=env)
    time.sleep(1)
    tone = subprocess.Popen(['ffmpeg', '-hide_banner', '-loglevel', 'error', '-f', 'lavfi',
          '-i', 'aevalsrc=0.25*sin(2*PI*440*t)|0.25*sin(2*PI*880*t):s=48000',
          '-f', 's32le', 'pipe:1'], stdout=subprocess.PIPE)
    children.append(tone)
    start(['aplay', '-D', 'inferno', '-t', 'raw', '-c', '2', '-r', '48000',
           '-f', 'S32_LE', '--buffer-size=8192', '--period-size=480'], 'inferno', env=env, stdin=tone.stdout)
    tone.stdout.close()
    time.sleep(3)
    start(['python3', '-u', '/test/relay.py'], 'relay')
    print('READY: Inferno 9767558, stereo 440/880 Hz, 48kHz', flush=True)
    while True:
        time.sleep(1)
        assert all(p.poll() is None for p in children), 'sender process exited'
finally:
    for p in reversed(children):
        if p.poll() is None:
            p.terminate()
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait(timeout=5)
    for log in logs:
        log.close()
