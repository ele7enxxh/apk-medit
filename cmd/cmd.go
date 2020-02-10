package cmd

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"

	sys "golang.org/x/sys/unix"
)

var tids []int
var isAttached = false

func Plist() (string, error) {
	cmd := exec.Command("ps", "-e")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}

	re := regexp.MustCompile(`\s+`)
	line, err := out.ReadString('\n')
	pids := []string{}
	for err == nil && len(line) != 0 {
		s := strings.Split(re.ReplaceAllString(string(line), " "), " ")
		pid := s[1]
		cmd := s[8]
		if pid != "PID" && cmd != "" && cmd != "ps" && cmd != "sh" && cmd != "medit" {
			fmt.Printf("Target Package: %s, PID: %s\n", cmd, pid)
			pids = append(pids, pid)
		}
		line, err = out.ReadString('\n')
	}

	if len(pids) == 1 {
		fmt.Printf("Attach target PID has been set to %s.\n", pids[0])
		return pids[0], nil
	}
	return "", nil
}

func Attach(pid string) error {
	if isAttached {
		fmt.Println("Already attached.")
		return nil
	}

	fmt.Printf("Target PID: %s\n", pid)
	tid_dir := fmt.Sprintf("/proc/%s/task", pid)
	if _, err := os.Stat(tid_dir); err == nil {
		tidinfo, err := ioutil.ReadDir(tid_dir)
		if err != nil {
			log.Fatal(err)
		}

		tids = []int{}
		for _, t := range tidinfo {
			tid, _ := strconv.Atoi(t.Name())
			tids = append(tids, tid)
		}

		for _, tid := range tids {
			if err := sys.PtraceAttach(tid); err == nil {
				fmt.Printf("Attached TID: %d\n", tid)
			} else {
				fmt.Printf("attach failed: %s\n", err)
			}
			if err := wait(tid); err != nil {
				fmt.Printf("Failed wait TID: %d, %s\n", tid, err)
			}
		}

		isAttached = true

	} else if os.IsNotExist(err) {
		fmt.Println("PID must be an integer that exists.")
	}
	return nil
}

func Find(pid string, targetVal uint64) ([][2]int, error) {
	// search value in /proc/<pid>/mem
	mapsPath := fmt.Sprintf("/proc/%s/maps", pid)
	addrRanges, err := getWritableAddrRanges(mapsPath)
	if err != nil {
		return nil, err
	}

	memPath := fmt.Sprintf("/proc/%s/mem", pid)
	foundAddrs, _ := findDataInAddrRanges(memPath, targetVal, addrRanges)
	fmt.Printf("Found: 0x%x!!!\n", len(foundAddrs))

	return foundAddrs, nil
}

func Filter(pid string, targetVal uint64, prevAddrs [][2]int) ([][2]int, error) {
	// In case targetVal length is bigger prev value, resize prevAddrs
	memPath := fmt.Sprintf("/proc/%s/mem", pid)
	foundAddrs, _ := findDataInAddrRanges(memPath, targetVal, prevAddrs)
	fmt.Printf("Found: 0x%x!!!\n", len(foundAddrs))
	return foundAddrs, nil
}

func getWritableAddrRanges(mapsPath string) ([][2]int, error) {
	addrRanges := [][2]int{}
	ignorePaths := []string{"/vendor/lib64/", "/system/lib64/", "/system/bin/", "/system/framework/", "/data/dalvik-cache/"}
	file, err := os.Open(mapsPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		meminfo := strings.Fields(line)
		addrRange := meminfo[0]
		permission := meminfo[1]
		if permission[0] == 'r' && permission[1] == 'w' && permission[3] != 's' {
			ignoreFlag := false
			if len(meminfo) >= 6 {
				filePath := meminfo[5]
				for _, ignorePath := range ignorePaths {
					if strings.HasPrefix(filePath, ignorePath) {
						ignoreFlag = true
						break
					}
				}
			}

			if !ignoreFlag {
				addrs := strings.Split(addrRange, "-")
				beginAddr, _ := strconv.ParseInt(addrs[0], 16, 64)
				endAddr, _ := strconv.ParseInt(addrs[1], 16, 64)
				addrRanges = append(addrRanges, [2]int{int(beginAddr), int(endAddr)})
			}
		}
	}
	return addrRanges, nil
}

var splitSize = 0x1000000
var bufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, splitSize)
	},
}

func findDataInAddrRanges(memPath string, targetVal uint64, addrRanges [][2]int) ([][2]int, error) {
	// TODO: Support UTF8 strings
	foundAddrs := [][2]int{}
	f, err := os.Open(memPath)
	defer f.Close()

	searchBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(searchBytes[0:], targetVal)
	searchLength := len(searchBytes)
	for _, s := range addrRanges {
		beginAddr := s[0]
		endAddr := s[1]
		memSize := endAddr - beginAddr
		if err != nil {
			fmt.Println(err)
		}
		for i := 0; i < (memSize/splitSize)+1; i++ {
			splitIndex := (i + 1) * splitSize
			splittedBeginAddr := beginAddr + i*splitSize
			splittedEndAddr := endAddr
			if splitIndex < memSize {
				splittedEndAddr = beginAddr + splitIndex
			}
			b := bufferPool.Get().([]byte)
			readMemory(f, b, splittedBeginAddr, splittedEndAddr)
			fmt.Printf("Memory size: 0x%x bytes\n", len(b))
			fmt.Printf("Begin Address: 0x%x, End Address 0x%x\n", splittedBeginAddr, splittedEndAddr)
			findDataInSplittedMemory(&b, searchBytes, searchLength, splittedBeginAddr, 0, &foundAddrs)
			bufferPool.Put(b)
			if len(foundAddrs) > 10000000 {
				fmt.Println("Too many addresses with target data found...")
				goto FINISH
			}
		}
	}

FINISH:
	return foundAddrs, nil
}

func findDataInSplittedMemory(memory *[]byte, searchBytes []byte, searchLength int, beginAddr int, offset int, results *[][2]int) {
	index := bytes.Index((*memory)[offset:], searchBytes)
	if index == -1 {
		return
	} else {
		resultAddr := beginAddr + index + offset
		*results = append(*results, [2]int{resultAddr, resultAddr + searchLength - 1})
		offset += index + searchLength
		findDataInSplittedMemory(memory, searchBytes, searchLength, beginAddr, offset, results)
	}
}

func readMemory(memFile *os.File, buffer []byte, beginAddr int, endAddr int) []byte {
	n := endAddr - beginAddr
	r := io.NewSectionReader(memFile, int64(beginAddr), int64(n))
	buffer = buffer[:n]
	r.Read(buffer)
	return buffer
}

/*
func writeMemory(memFile *os.File, targetAddr int, tagerVal int) ([]byte, error) {
	f, err := os.Open(memPath)
	if err != nil {
		panic(err)
	}

	return buffer, nil
}
*/

func Detach() error {
	if !isAttached {
		fmt.Println("Already detached.")
		return nil
	}

	for _, tid := range tids {
		if err := sys.PtraceDetach(tid); err != nil {
			return fmt.Errorf("%d detach failed. %s\n", tid, err)
		} else {
			fmt.Printf("Detached TID: %d\n", tid)
		}
	}

	isAttached = false
	return nil
}

func wait(pid int) error {
	var s sys.WaitStatus

	// sys.WALL = 0x40000000 on Linux(ARM64)
	// Using sys.WALL does not pass test on macOS.
	// https://github.com/golang/go/blob/50bd1c4d4eb4fac8ddeb5f063c099daccfb71b26/src/syscall/zerrors_linux_arm.go#L1203
	wpid, err := sys.Wait4(pid, &s, 0x40000000, nil)
	if err != nil {
		return err
	}

	if wpid != pid {
		return fmt.Errorf("wait failed: wpid = %d", wpid)
	}
	if !s.Stopped() {
		return fmt.Errorf("wait failed: status is not stopped: ")
	}

	return nil
}
