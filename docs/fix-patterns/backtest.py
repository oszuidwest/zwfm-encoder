import csv,json,collections,sys
fx={f['sha']:f for f in json.load(open('mine/fixes.json'))}
pairs=list(csv.DictReader(open('mine/pairs.csv')))
def rank(v):
    s=sorted(range(len(v)),key=lambda i:v[i]); r=[0]*len(v); i=0
    while i<len(s):
        j=i
        while j+1<len(s) and v[s[j+1]]==v[s[i]]: j+=1
        for k in range(i,j+1): r[s[k]]=(i+j)/2
        i=j+1
    return r
def spearman(a,b):
    ra,rb=rank(a),rank(b); n=len(a); ma=sum(ra)/n; mb=sum(rb)/n
    num=sum((x-ma)*(y-mb) for x,y in zip(ra,rb)); den=(sum((x-ma)**2 for x in ra)*sum((y-mb)**2 for y in rb))**.5
    return num/den if den else 0
for cut in ('2026-06-01','2026-08-01'):
    pre={r['module']:r for r in csv.DictReader(open(f'bt-{cut[5:7]}01/per_module.csv'))}
    post_fix=collections.Counter(); post_ff=collections.Counter()
    for f in fx.values():
        if f['date']>=cut:
            for m in f['modules']: post_fix[m]+=1
    for p in pairs:
        if fx[p['fix']]['date']>=cut: post_ff[(p['fix'],p['module'])]=1
    pff=collections.Counter(m for (_,m) in post_ff)
    mods=[m for m in pre if m not in ('output','other') and float(pre[m]['loc'] or 0)>0]
    print(f'== cutoff {cut}: {len(mods)} modules; post-cutoff fixes={sum(1 for f in fx.values() if f["date"]>=cut)}')
    preds={
      'loc (null model)':[float(pre[m]['loc']) for m in mods],
      'pre fix commits':[float(pre[m]['fix_commits']) for m in mods],
      'pre fix_induced':[float(pre[m]['fix_induced']) for m in mods],
      'pre fix_induced+lenient':[float(pre[m]['fix_induced_lenient']) for m in mods],
      'pre file revisions':[float(pre[m]['file_revisions']) for m in mods],
      'pre fixes/kloc':[float(pre[m]['fixes_per_kloc'] or 0) for m in mods],
      'pre observed fix-share':[float(pre[m]['observed'] or 0) for m in mods],
    }
    for outname,out in (('post fixes',[post_fix[m] for m in mods]),('post fix-on-fix',[pff[m] for m in mods])):
        print(' outcome:',outname)
        for k,v in preds.items(): print(f'   rho={spearman(v,out):+.2f}  {k}')
    top=sorted(mods,key=lambda m:-float(pre[m]['fix_induced']))[:5]
    print(' top5 by pre fix_induced:',[(m,pre[m]['fix_induced'],post_fix[m],pff[m]) for m in top])
    print(' top5 post fix-on-fix   :',pff.most_common(5))
