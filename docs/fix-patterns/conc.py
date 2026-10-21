import os,re,sys,collections,json
PAT={'mutex':r'sync\.(RW)?Mutex','go':r'\bgo (func\b|[A-Za-z_][\w.]*\()','timer':r'time\.(AfterFunc|NewTimer|NewTicker)','ctx':r'context\.With(Cancel|Timeout|Deadline)','atomic':r'\batomic\.','chan':r'\bchan\b','select':r'\bselect \{','flag':r'\b(stopping|stopped|closed|closing|running|started|shutdown|shuttingDown)\b\s+bool|\b(gen|generation|runID|epoch)\b\s+(u?int|uint64)'}
def module(p):
    a=p.split('/')
    if a[0]=='internal' and len(a)>2: return a[1]
    if len(a)==1: return 'root'
    return 'other'
def scan(root):
    res=collections.defaultdict(collections.Counter)
    for d,_,fs in os.walk(root):
        if any(x in d for x in ('/node_modules','/.git','/tests','/vendor')): continue
        for f in fs:
            if not f.endswith('.go') or f.endswith('_test.go'): continue
            p=os.path.relpath(os.path.join(d,f),root); t=open(os.path.join(d,f),errors='replace').read()
            m=module(p)
            res[m]['loc']+=sum(1 for l in t.splitlines() if l.strip())
            for k,r in PAT.items(): res[m][k]+=len(re.findall(r,t))
            res[m]['conc']=sum(res[m][k] for k in PAT)
    return res
out={k:scan(v) for k,v in (('0601',sys.argv[1]),('0801',sys.argv[2]),('head',sys.argv[3]))}
json.dump(out,open('conc.json','w'))
h=out['head']
print(f"{'module':12} {'loc':>5} "+' '.join(f'{k:>6}' for k in PAT)+'   conc  /kloc')
for m in sorted(h,key=lambda m:-h[m]['conc']):
    print(f"{m:12} {h[m]['loc']:5} "+' '.join(f'{h[m][k]:6}' for k in PAT)+f"  {h[m]['conc']:5}  {h[m]['conc']/h[m]['loc']*1000:5.1f}")
