package util

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dss "github.com/ipfs/go-datastore/sync"
	"github.com/ipfs/go-filestore"
	bstore "github.com/ipfs/go-ipfs-blockstore"
	offline "github.com/ipfs/go-ipfs-exchange-offline"
	files "github.com/ipfs/go-ipfs-files"
	format "github.com/ipfs/go-ipld-format"
	ipld "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log/v2"
	"github.com/ipfs/go-merkledag"
	dag "github.com/ipfs/go-merkledag"
	"github.com/ipfs/go-unixfs"
	"github.com/ipfs/go-unixfs/importer/balanced"
	"github.com/ipfs/go-unixfs/importer/helpers"
	ihelper "github.com/ipfs/go-unixfs/importer/helpers"
	"github.com/ipld/go-car"
	ipldprime "github.com/ipld/go-ipld-prime"
	basicnode "github.com/ipld/go-ipld-prime/node/basic"
	"github.com/ipld/go-ipld-prime/traversal/selector"
	"github.com/ipld/go-ipld-prime/traversal/selector/builder"
	"golang.org/x/xerrors"

	"github.com/dataswap/go-metadata/libs"
	metaservice "github.com/dataswap/go-metadata/service"
	pb "github.com/ipfs/go-unixfs/pb"
)

const UnixfsLinksPerLevel = 1 << 10
const UnixfsChunkSize uint64 = 1 << 20

var logger = logging.Logger("graphsplit")

type FSBuilder struct {
	root *dag.ProtoNode
	ds   ipld.DAGService
}

type CidMapValue struct {
	IsDir bool
	Cid   string
}

func getDirKey(dirList []string, i int) (key string) {
	for j := 0; j <= i; j++ {
		key += dirList[j]
		if j < i {
			key += "."
		}
	}
	return
}
func NewFSBuilder(root *dag.ProtoNode, ds ipld.DAGService) *FSBuilder {
	return &FSBuilder{root, ds}
}

func isLinked(node *dag.ProtoNode, name string) bool {
	for _, lk := range node.Links() {
		if lk.Name == name {
			return true
		}
	}
	return false
}

type Finfo struct {
	Path  string
	Size  int64
	Start int64
	End   int64
}

type fileSlice struct {
	r        *os.File
	offset   int64
	start    int64
	end      int64
	fileSize int64
}

func (fs *fileSlice) Close() error {
	return fs.Close()
}

func (fs *fileSlice) Read(p []byte) (n int, err error) {
	if fs.end == 0 {
		fs.end = fs.fileSize
	}
	if fs.offset == 0 && fs.start > 0 {
		_, err = fs.r.Seek(fs.start, 0)
		if err != nil {
			logger.Warn(err)
			return 0, err
		}
		fs.offset = fs.start
	}
	if fs.end-fs.offset == 0 {
		return 0, io.EOF
	}
	if fs.end-fs.offset < 0 {
		return 0, xerrors.Errorf("read data out bound of the slice")
	}
	plen := len(p)
	leftLen := fs.end - fs.offset
	if leftLen > int64(plen) {
		n, err = fs.r.Read(p)
		if err != nil {
			return
		}
		fs.offset += int64(n)
		return
	}
	b := make([]byte, leftLen)
	n, err = fs.r.Read(b)
	if err != nil {
		return
	}
	fs.offset += int64(n)

	return copy(p, b), io.EOF
}

func GenerateCar(ctx context.Context, fileList []Finfo, parentPath string, tmpDir string, output io.Writer, msrv *metaservice.MetaService) (ipldDag *FsNode, cid string, cidMap map[string]CidMapValue, err error) {
	batching := dss.MutexWrap(datastore.NewMapDatastore())
	bs1 := bstore.NewBlockstore(batching)
	absParentPath, err := filepath.Abs(parentPath)
	cidMap = make(map[string]CidMapValue)
	if err != nil {
		logger.Warn(err)
		return
	}
	if tmpDir != "" {
		absParentPath, err = filepath.Abs(tmpDir)
		if err != nil {
			logger.Warn(err)
			return
		}
	}
	fm := filestore.NewFileManager(batching, absParentPath)
	fm.AllowFiles = true
	bs2 := filestore.NewFilestore(bs1, fm)

	dagServ1 := merkledag.NewDAGService(blockservice.New(bs2, offline.Exchange(bs2)))
	dagServ := msrv.GenDagService(dagServ1)
	cidBuilder, err := merkledag.PrefixForCidVersion(1)
	if err != nil {
		logger.Warn(err)
		return
	}
	var layers []interface{}
	//rootNode := uio.NewDirectory(dagServ)
	//rootNode.SetCidBuilder(cidBuilder)

	rootNode := helpers.NewFSNodeOverDag(pb.Data_Directory, cidBuilder)
	layers = append(layers, rootNode)
	previous := []string{""}
	for _, item := range fileList {
		if _, err := os.Stat(item.Path); err != nil {
			return nil, "", nil, err
		}
		if item.End == 0 {
			item.End = item.Size
		}
		var node ipld.Node
		var path string
		path, err = filepath.Rel(filepath.Clean(parentPath), filepath.Clean(item.Path))
		if tmpDir != "" {
			tmpPath := filepath.Join(filepath.Clean(tmpDir), path)
			err = os.MkdirAll(filepath.Dir(tmpPath), 0777)
			if err != nil {
				logger.Warn(err)
				return
			}
			// copy file
			var source, destination *os.File
			source, err = os.Open(item.Path)
			if err != nil {
				return
			}
			defer source.Close()
			destination, err = os.Create(tmpPath)
			if err != nil {
				return
			}
			defer destination.Close()
			_, err = source.Seek(item.Start, 0)
			if err != nil {
				return
			}
			_, err = io.CopyN(destination, source, item.End-item.Start)
			if err != nil {
				return
			}
			item.Path = tmpPath
			item.Size = item.End - item.Start
			item.End = item.Size
			item.Start = 0
		}

		node, err = BuildFileNode(ctx, item, dagServ, cidBuilder, msrv, parentPath)
		if err != nil {
			return
		}
		err = dagServ.Add(ctx, node)
		if err != nil {
			return
		}
		cidMap[strings.Join(strings.Split(path, string(filepath.Separator)), "/")] = CidMapValue{false, node.Cid().String()}
		current := append([]string{""}, strings.Split(path, string(filepath.Separator))...)
		// Find the common prefix
		i := 0
		var minLength int
		if len(previous) < len(current) {
			minLength = len(previous)
		} else {
			minLength = len(current)
		}
		for ; i < minLength; i++ {
			if previous[i] != current[i] {
				break
			}
		}
		for j := len(previous) - 1; j >= i; j-- {
			lastNode := layers[len(layers)-1]
			lastName := previous[len(previous)-1]
			layers = layers[:len(layers)-1]
			dirNode, ok := layers[len(layers)-1].(*helpers.FSNodeOverDag)
			if !ok {
				err = xerrors.Errorf("node is not directory")
				return
			}
			lastDirNode, ok := lastNode.(*helpers.FSNodeOverDag)
			if ok {
				var n ipld.Node
				//childFileSize := lastDirNode.FileSize()
				n, err = (*lastDirNode).Commit()
				if err != nil {
					return
				}
				err = dagServ.Add(ctx, n)
				if err != nil {
					return
				}
				var stat *ipld.NodeStat
				stat, err = n.Stat()
				if err != nil {
					return
				}
				childFileSize := uint64(stat.BlockSize)
				cidMap[strings.Join(previous[1:], "/")] = CidMapValue{true, n.Cid().String()}
				err = (*dirNode).AddChildToFsNode(n, childFileSize, lastName)
				if err != nil {
					return nil, "", nil, err
				}
				//fmt.Println("AddChildLink name:", lastName, " cid:", n.Cid().String(), " file size:", childFileSize, " generate-car")
			} else {
				lastFileNode, _ := lastNode.(ipld.Node)
				var stat *ipld.NodeStat
				stat, err = lastFileNode.Stat()
				if err != nil {
					return
				}
				childFileSize := uint64(stat.BlockSize)

				//err = (*dirNode).AddChild(ctx, lastName, lastFileNode)
				err = (*dirNode).AddChildToFsNode(lastFileNode, childFileSize, lastName)
				if err != nil {
					return nil, "", nil, err
				}
				//fmt.Println("AddChildLink name:", lastName, " cid:", lastFileNode.Cid().String(), " file size:", childFileSize, " generate-car")
			}
			previous = previous[:len(previous)-1]
		}
		for j := i; j < len(current); j++ {
			if j == len(current)-1 {
				layers = append(layers, node)
			} else {
				//newNode := uio.NewDirectory(dagServ)
				newNode := helpers.NewFSNodeOverDag(pb.Data_Directory, cidBuilder)
				//newNode.SetCidBuilder(cidBuilder)
				layers = append(layers, newNode)
			}
		}
		previous = current
	}
	for j := len(previous) - 1; j >= 1; j-- {
		lastNode := layers[len(layers)-1]
		lastName := previous[len(previous)-1]
		layers = layers[:len(layers)-1]
		//dirNode, ok := layers[len(layers)-1].(*uio.Directory)
		dirNode, ok := layers[len(layers)-1].(*helpers.FSNodeOverDag)
		if !ok {
			err = xerrors.Errorf("node is not directory")
			return
		}
		lastDirNode, ok := lastNode.(*helpers.FSNodeOverDag)
		if ok {
			var n ipld.Node
			//childFileSize := lastDirNode.FileSize()
			n, err = (*lastDirNode).Commit()
			if err != nil {
				return
			}
			err = dagServ.Add(ctx, n)
			if err != nil {
				return
			}
			var stat *ipld.NodeStat
			stat, err = n.Stat()
			if err != nil {
				return
			}
			childFileSize := uint64(stat.BlockSize)

			cidMap[strings.Join(previous[1:], "/")] = CidMapValue{true, n.Cid().String()}
			err = (*dirNode).AddChildToFsNode(n, childFileSize, lastName)
			if err != nil {
				return
			}
			//fmt.Println("AddChildLink name:", lastName, " cid:", n.Cid().String(), " file size:", childFileSize, " generate-car")
		} else {
			lastFileNode, _ := lastNode.(ipld.Node)
			var stat *ipld.NodeStat
			stat, err = lastFileNode.Stat()
			if err != nil {
				return
			}
			childFileSize := uint64(stat.BlockSize)

			//err = (*dirNode).AddChild(ctx, lastName, lastFileNode)
			err = (*dirNode).AddChildToFsNode(lastFileNode, childFileSize, lastName)
			if err != nil {
				return
			}
			//fmt.Println("AddChildLink name:", lastName, " cid:", lastFileNode.Cid().String(), " file size:", childFileSize, " generate-car")
		}
		previous = previous[:len(previous)-1]
	}
	//rootIpldNode, _ := rootNode.GetNode()
	rootIpldNode, _ := rootNode.Commit()
	err = dagServ.Add(ctx, rootIpldNode)
	if err != nil {
		return
	}
	cidMap[""] = CidMapValue{true, rootIpldNode.Cid().String()}
	selector := allSelector()
	sc := car.NewSelectiveCar(ctx, bs2, []car.Dag{{Root: rootIpldNode.Cid(), Selector: selector}})
	err = sc.Write(
		msrv.GenCarWriter(output, "", true),
	)
	if err != nil {
		return
	}
	rootProtoNode, ok := rootIpldNode.(*dag.ProtoNode)
	if !ok {
		err = xerrors.Errorf("node is not proto node")
		return
	}
	fsBuilder := NewFSBuilder(rootProtoNode, dagServ)
	ipldDag, err = fsBuilder.Build()
	if err != nil {
		return
	}
	cid = rootIpldNode.Cid().String()
	msrv.SetCarRoot(rootIpldNode.Cid())
	return
}

func allSelector() ipldprime.Node {
	ssb := builder.NewSelectorSpecBuilder(basicnode.Prototype.Any)
	return ssb.ExploreRecursive(selector.RecursionLimitNone(),
		ssb.ExploreAll(ssb.ExploreRecursiveEdge())).
		Node()
}

func BuildFileNode(ctx context.Context, item Finfo, bufDs ipld.DAGService, cidBuilder cid.Builder, msrv *metaservice.MetaService, parent string) (node ipld.Node, err error) {
	f, err := os.Open(item.Path)
	if err != nil {
		logger.Warn(err)
		return
	}
	var r io.Reader
	if item.Start == 0 && item.End == item.Size {
		r, err = files.NewReaderPathFile(item.Path, f, nil)
	} else {
		r, err = files.NewReaderPathFile(item.Path, &fileSlice{
			r:        f,
			start:    item.Start,
			end:      item.End,
			fileSize: item.Size,
		}, nil)
	}
	if err != nil {
		logger.Warn(err)
		return
	}

	params := ihelper.DagBuilderParams{
		Maxlinks:   UnixfsLinksPerLevel,
		RawLeaves:  false,
		CidBuilder: cidBuilder,
		Dagserv:    bufDs,
		NoCopy:     true,
	}
	var db helpers.Helper
	var spl libs.EnhancedSplitter
	spl, err = libs.NewSplitter(r, int64(UnixfsChunkSize), item.Path, parent)
	if err != nil {
		return
	}
	if msrv != nil {
		db, err = msrv.GenHelper(&params, spl)
	} else {
		db, err = params.New(spl)
	}
	db.SetOffset(uint64(item.Start))
	if err != nil {
		logger.Warn(err)
		return
	}
	node, err = balanced.Layout(db)
	if err != nil {
		logger.Warn(err)
		return
	}
	return
}
func (b *FSBuilder) Build() (rootn *FsNode, err error) {
	fsn, err := unixfs.FSNodeFromBytes(b.root.Data())
	if err != nil {
		return nil, xerrors.Errorf("input dag is not a unixfs node: %s", err)
	}

	rootn = &FsNode{
		Hash: b.root.Cid().String(),
		Size: fsn.FileSize(),
		Link: []FsNode{},
	}
	if !fsn.IsDir() {
		return rootn, nil
	}
	for _, ln := range b.root.Links() {
		var fn FsNode
		fn, err = b.getNodeByLink(ln)
		if err != nil {
			logger.Warn(err)
			return
		}
		rootn.Link = append(rootn.Link, fn)
	}

	return rootn, nil
}

type FsNode struct {
	Name string
	Hash string
	Size uint64
	Link []FsNode
}

func (b *FSBuilder) getNodeByLink(ln *format.Link) (fn FsNode, err error) {
	ctx := context.Background()
	fn = FsNode{
		Name: ln.Name,
		Hash: ln.Cid.String(),
		Size: ln.Size,
	}
	nd, err := b.ds.Get(ctx, ln.Cid)
	if err != nil {
		logger.Warn(err)
		return
	}

	nnd, ok := nd.(*dag.ProtoNode)
	if !ok {
		// format.Node | merkeldag.RawNode
		return
	}
	fsn, err := unixfs.FSNodeFromBytes(nnd.Data())
	if err != nil {
		logger.Warn("input dag is not a unixfs node: %s", err)
		return
	}
	if !fsn.IsDir() {
		return
	}
	for _, ln := range nnd.Links() {
		node, err := b.getNodeByLink(ln)
		if err != nil {
			return node, err
		}
		fn.Link = append(fn.Link, node)
	}
	return
}
