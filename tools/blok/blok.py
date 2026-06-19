import numpy as np, sys
from PIL import Image

def load(path): return np.array(Image.open(path).convert('RGB')).astype(int)

def detect_board(im):
    H,W,_=im.shape; R,G,B=im[:,:,0],im[:,:,1],im[:,:,2]
    dark=(R<150)&(R>50)&(G<80)&(B<80)
    col=dark[int(0.10*H):int(0.50*H),:].sum(0)
    cols=np.where(col>col.max()*0.5)[0]; x0,x1=int(cols.min()),int(cols.max())
    rr=dark[:,x0:x1+1].sum(1); rows=np.where(rr>rr.max()*0.5)[0]
    return x0,int(rows.min()),(x1-x0)/8.0

def _empty(rgb):
    r,g,b=rgb; return r<155 and g<60 and b<55

def read_board(im):
    x0,y0,cell=detect_board(im); g=[[0]*8 for _ in range(8)]; col=[[None]*8 for _ in range(8)]
    h=max(2,int(cell*0.16))
    for r in range(8):
        for c in range(8):
            cx,cy=x0+(c+0.5)*cell,y0+(r+0.5)*cell
            m=tuple(int(v) for v in im[int(cy)-h:int(cy)+h,int(cx)-h:int(cx)+h].reshape(-1,3).mean(0))
            col[r][c]=m; g[r][c]=0 if _empty(m) else 1
    return g,(x0,y0,cell),col

def _piece_mask(im):
    R,G,B=im[:,:,0],im[:,:,1],im[:,:,2]; return (B>120)|(G>180)

def _bands(proj):
    """runs where proj exceeds 35% of its max -> list of (start,end)."""
    if proj.max()==0: return []
    on=proj>proj.max()*0.18; bands=[]; s=None
    for i,v in enumerate(on):
        if v and s is None: s=i
        elif not v and s is not None: bands.append((s,i-1)); s=None
    if s is not None: bands.append((s,len(on)-1))
    return bands

def read_pieces(im, board):
    x0,y0,cell=board; mask=_piece_mask(im)
    tray=np.zeros_like(mask); tray[int(y0+8.3*cell):int(y0+11.5*cell),:]=True
    m=mask&tray; cols=np.where(m.any(0))[0]
    if len(cols)==0: return []
    groups=[]; cur=[cols[0]]
    for x in cols[1:]:
        if x-cur[-1]<=0.4*cell: cur.append(x)
        else: groups.append(cur); cur=[x]
    groups.append(cur)
    pieces=[]
    for g in groups:
        sub=m[:,g[0]:g[-1]+1]; rows=np.where(sub.any(1))[0]
        gx0,gx1,gy0,gy1=g[0],g[-1],rows.min(),rows.max()
        box=m[gy0:gy1+1,gx0:gx1+1]
        cb=_bands(box.sum(0)); rb=_bands(box.sum(1)); nc,nr=len(cb),len(rb)
        shape=[[0]*nc for _ in range(nr)]
        for i,(rs,re) in enumerate(rb):
            for j,(cs,ce) in enumerate(cb):
                shape[i][j]=1 if box[rs:re+1,cs:ce+1].mean()>0.35 else 0
        cx,cy=(gx0+gx1)//2,(gy0+gy1)//2
        pieces.append({'shape':shape,'h':nr,'w':nc,'cells':int(box.mean()and sum(sum(r) for r in shape)),'pick_vp':(cx//2,cy//2)})
    return pieces

if __name__=="__main__":
    im=load(sys.argv[1]); mode=sys.argv[2] if len(sys.argv)>2 else "board"
    g,board,col=read_board(im)
    if mode!="pieces":
        for row in g: print("".join("#" if v else "." for v in row))
    if mode in("pieces","all"):
        for i,p in enumerate(read_pieces(im,board)):
            print(f"piece{i}: {p['h']}x{p['w']} cells={sum(sum(r) for r in p['shape'])} pick_vp={p['pick_vp']}")
            for row in p['shape']: print("   "+"".join("#" if v else "." for v in row))
