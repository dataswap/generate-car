package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"

	commcid "github.com/filecoin-project/go-fil-commcid"
	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	cbor "github.com/ipfs/go-ipld-cbor"
	"github.com/tech-greedy/generate-car/util"
	"github.com/urfave/cli/v2"

	metaservice "github.com/dataswap/go-metadata/service"
)

type CommpResult struct {
	commp     string
	pieceSize uint64
}

type Result struct {
	Ipld      *util.FsNode
	DataCid   string
	PieceCid  string
	PieceSize uint64
	CidMap    map[string]util.CidMapValue
}

type Input []util.Finfo

type CarHeader struct {
	Roots   []cid.Cid
	Version uint64
}

func init() {
	cbor.RegisterCborType(CarHeader{})
}

const BufSize = (4 << 20) / 128 * 127

func main() {
	ctx := context.TODO()
	app := &cli.App{
		Name:  "generate-car",
		Usage: "generate car archive from list of files and compute commp in the mean time",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "single",
				Usage: "When enabled, it indicates that the input is a single file or folder to be included in full, instead of a spec JSON",
			},
			&cli.StringFlag{
				Name:    "input",
				Aliases: []string{"i"},
				Usage:   "When --single is specified, this is the file or folder to be included in full. Otherwise this is a JSON file containing the list of files to be included in the car archive",
				Value:   "-",
			},
			&cli.Uint64Flag{
				Name:    "piece-size",
				Aliases: []string{"s"},
				Usage:   "Target piece size, default to minimum possible value",
				Value:   0,
			},
			&cli.StringFlag{
				Name:    "out-dir",
				Aliases: []string{"o"},
				Usage:   "Output directory to save the car file",
				Value:   ".",
			},
			&cli.StringFlag{
				Name:    "tmp-dir",
				Aliases: []string{"t"},
				Usage:   "Optionally copy the files to a temporary (and much faster) directory",
				Value:   "",
			},
			&cli.StringFlag{
				Name:     "parent",
				Aliases:  []string{"p"},
				Usage:    "Parent path of the dataset",
				Required: true,
			},
		},
		Action: func(c *cli.Context) error {
			inputFile := c.String("input")
			pieceSizeInput := c.Uint64("piece-size")
			outDir := c.String("out-dir")
			parent := c.String("parent")
			tmpDir := c.String("tmp-dir")
			single := c.Bool("single")

			var input Input
			if single {
				stat, err := os.Stat(inputFile)
				if err != nil {
					return err
				}
				if stat.IsDir() {
					err := filepath.Walk(inputFile, func(path string, info os.FileInfo, err error) error {
						if err != nil {
							return err
						}
						if info.IsDir() {
							return nil
						}
						input = append(input, util.Finfo{
							Path:  path,
							Size:  info.Size(),
							Start: 0,
							End:   info.Size(),
						})
						return nil
					})
					if err != nil {
						return err
					}
				} else {
					input = append(input, util.Finfo{
						Path:  inputFile,
						Size:  stat.Size(),
						Start: 0,
						End:   stat.Size(),
					})
				}
			} else {
				var inputBytes []byte
				if inputFile == "-" {
					reader := bufio.NewReader(os.Stdin)
					buf := new(bytes.Buffer)
					_, err := buf.ReadFrom(reader)
					if err != nil {
						return err
					}
					inputBytes = buf.Bytes()
				} else {
					bytes, err := os.ReadFile(inputFile)
					if err != nil {
						return err
					}
					inputBytes = bytes
				}
				err := json.Unmarshal(inputBytes, &input)
				if err != nil {
					return err
				}
			}

			outFilename := uuid.New().String() + ".car"
			outPath := path.Join(outDir, outFilename)
			carF, err := os.Create(outPath)
			if err != nil {
				return err
			}
			buf := bytes.Buffer{}
			writer := bufio.NewWriterSize(io.MultiWriter(carF, &buf), BufSize)

			msrv := metaservice.New()
			ipld, cid, cidMap, err := util.GenerateCar(ctx, input, parent, tmpDir, writer, msrv)
			if err != nil {
				return err
			}
			err = writer.Flush()
			if err != nil {
				return err
			}
			err = carF.Close()
			if err != nil {
				return err
			}
			cachePath := path.Join(outDir, metaservice.METAS_PATH, metaservice.PROOFS_PATH)
			rawCommP, pieceSize, err := metaservice.GenCommP(buf, cachePath, pieceSizeInput)
			if err != nil {
				return err
			}

			metaservice.SaveCommP(rawCommP, uint64(buf.Len()), cachePath)
			commCid, err := commcid.DataCommitmentV1ToCID(rawCommP)
			if err != nil {
				return err
			}
			err = os.Rename(outPath, path.Join(outDir, commCid.String()+".car"))
			if err != nil {
				return err
			}

			err = os.Rename(path.Join(cachePath, hex.EncodeToString(rawCommP)+metaservice.CACHE_SUFFIX), path.Join(cachePath, commCid.String()+metaservice.CACHE_SUFFIX))
			if err != nil {
				return err
			}

			metaPath := path.Join(outDir, metaservice.METAS_PATH, metaservice.MAPPINGS_PATH)
			msrv.SaveMetaMappings(metaPath, commCid.String()+metaservice.MAPPING_FILE_SUFFIX)

			output, err := json.Marshal(Result{
				Ipld:      ipld,
				DataCid:   cid,
				PieceCid:  commCid.String(),
				PieceSize: pieceSize,
				CidMap:    cidMap,
			})
			if err != nil {
				return err
			}
			fmt.Println(string(output))
			return nil
		},
	}
	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}
