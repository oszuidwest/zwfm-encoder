import os
import subprocess
import time

children = []
def start(args, name, **kwargs):
    log = open('/test/' + name + '.log', 'ab', buffering=0)
    p = subprocess.Popen(args, stderr=log, stdout=log, **kwargs)
    children.append(p)
    return p

# run.sh removes the container, which ends every child; no cleanup is needed here.
env = dict(os.environ, USRVCLOCK_SOCKET='/tmp/audit-clock',
           INFERNO_CLOCK_PATH='/tmp/audit-clock', INFERNO_NAME='inferno-audit',
           INFERNO_BIND_IP='eth0', INFERNO_SAMPLE_RATE='48000',
           INFERNO_TX_CHANNELS='2', INFERNO_RX_CHANNELS='2',
           INFERNO_TX_LATENCY_NS='50000000',
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
while True:
    time.sleep(1)
    assert all(p.poll() is None for p in children), 'sender process exited'
