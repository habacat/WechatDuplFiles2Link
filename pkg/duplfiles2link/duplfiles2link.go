package duplfiles2link

import (
	"bufio"
	"crypto/md5"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"
	"time"
	"sync/atomic"

	"golang.org/x/sys/windows"
)

const (
	Version = "0.0.2"
	Author  = "Qiu Sheng Designed"
)

type LogLevel string

const (
	LogLevelInfo  LogLevel = "INFO"
	LogLevelError LogLevel = "ERROR"
	LogLevelDebug LogLevel = "DEBUG"
)

type FileInfo struct {
	Path       string
	MD5Hash    string
	SHA256Hash string
	Size       int64
	ModTime    time.Time
}

type DuplProcessor struct {
	IsHardLink      bool
	wg              sync.WaitGroup
	mu              sync.Mutex
	fileMap         map[string][]FileInfo // MD5 hash -> file infos
	ProgressFunc    func(current, total int, message string)
	LogFunc         func(level LogLevel, message string)
	processedMap    map[string]bool // tracks already processed files
	processedCount  int32           // atomic counter for processed files
	totalFiles      int32           // atomic counter for total files
	maxConcurrency  int             // maximum number of concurrent goroutines
	workerSemaphore chan struct{}   // semaphore to limit concurrent operations
}

func GetVersion() string {
	return fmt.Sprintf("%s - %s", Version, Author)
}

func NewProcessor(isHardLink bool) *DuplProcessor {
	maxConcurrency := runtime.NumCPU() * 2
	
	return &DuplProcessor{
		IsHardLink:     isHardLink,
		fileMap:        make(map[string][]FileInfo),
		processedMap:   make(map[string]bool),
		maxConcurrency: maxConcurrency,
		workerSemaphore: make(chan struct{}, maxConcurrency),
		ProgressFunc: func(current, total int, message string) {
		},
		LogFunc: func(level LogLevel, message string) {
			fmt.Printf("[%s] %s\n", level, message)
		},
	}
}

func (dp *DuplProcessor) SetProgressCallback(fn func(current, total int, message string)) {
	dp.ProgressFunc = fn
}

func (dp *DuplProcessor) SetLogCallback(fn func(level LogLevel, message string)) {
	dp.LogFunc = fn
}

func (dp *DuplProcessor) Log(level LogLevel, message string) {
	if dp.LogFunc != nil {
		dp.LogFunc(level, message)
	}
}

func (dp *DuplProcessor) updateProgress(message string) {
	current := int(atomic.LoadInt32(&dp.processedCount))
	total := int(atomic.LoadInt32(&dp.totalFiles))
	
	if dp.ProgressFunc != nil && total > 0 {
		dp.ProgressFunc(current, total, message)
	}
}

func (dp *DuplProcessor) ProcessPath(rootPath string) error {
	info, err := os.Stat(rootPath)
	if err != nil {
		return fmt.Errorf("无法访问路径: %w", err)
	}

	if info.IsDir() {
		fileCount := int32(0)
		err = filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				atomic.AddInt32(&fileCount, 1)
			}
			return nil
		})
		
		if err != nil {
			return fmt.Errorf("计数文件时出错: %w", err)
		}
		
		atomic.StoreInt32(&dp.totalFiles, fileCount)
		dp.Log(LogLevelInfo, fmt.Sprintf("共发现 %d 个文件需要处理", fileCount))
		
		return filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				dp.Log(LogLevelError, fmt.Sprintf("访问 %s 时出错: %s", path, err))
				return nil
			}

			if !d.IsDir() {
				dp.mu.Lock()
				isProcessed := dp.processedMap[path]
				dp.mu.Unlock()
				
				if isProcessed {
					atomic.AddInt32(&dp.processedCount, 1)
					dp.updateProgress("扫描文件中...")
					return nil
				}
				
				dp.workerSemaphore <- struct{}{}
				dp.wg.Add(1)
				go func(filePath string) {
					defer dp.wg.Done()
					defer func() { <-dp.workerSemaphore }()
					
					dp.processFile(filePath)
					atomic.AddInt32(&dp.processedCount, 1)
					
					current := atomic.LoadInt32(&dp.processedCount)
					if current%20 == 0 || current == dp.totalFiles {
						dp.updateProgress("扫描文件中...")
					}
				}(path)
			}
			return nil
		})
	} else {
		dp.mu.Lock()
		isProcessed := dp.processedMap[rootPath]
		dp.mu.Unlock()
		
		if !isProcessed {
			atomic.StoreInt32(&dp.totalFiles, 1)
			dp.wg.Add(1)
			go func() {
				defer dp.wg.Done()
				dp.processFile(rootPath)
				atomic.StoreInt32(&dp.processedCount, 1)
				dp.updateProgress("扫描文件中...")
			}()
		}
	}
	return nil
}

func (dp *DuplProcessor) processFile(filePath string) {
	defer func() {
		if r := recover(); r != nil {
			dp.Log(LogLevelError, fmt.Sprintf("从处理文件 %s 的恐慌中恢复: %v", filePath, r))
		}
	}()

	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		dp.Log(LogLevelError, fmt.Sprintf("文件不存在: %s", filePath))
		return
	}

	isLink, err := IsFileLink(filePath)
	if err != nil {
		dp.Log(LogLevelError, fmt.Sprintf("检查文件是否为链接时出错 %s: %s", filePath, err))
		return
	}

	if isLink {
		dp.Log(LogLevelDebug, fmt.Sprintf("跳过已经是链接的文件: %s", filePath))
		dp.mu.Lock()
		dp.processedMap[filePath] = true
		dp.mu.Unlock()
		return
	}

	file, err := os.Open(filePath)
	if err != nil {
		dp.Log(LogLevelError, fmt.Sprintf("无法打开文件 %s: %s", filePath, err))
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		dp.Log(LogLevelError, fmt.Sprintf("无法获取文件信息 %s: %s", filePath, err))
		return
	}

	if info.Size() < 1024 {
		return
	}

	buff := make([]byte, 1)
	_, err = file.Read(buff)
	if err != nil {
		dp.Log(LogLevelError, fmt.Sprintf("文件不可读 %s: %s", filePath, err))
		return
	}

	_, err = file.Seek(0, 0)
	if err != nil {
		dp.Log(LogLevelError, fmt.Sprintf("无法重置文件指针 %s: %s", filePath, err))
		return
	}

	md5Hash, err := calculateMD5(file)
	if err != nil {
		dp.Log(LogLevelError, fmt.Sprintf("计算MD5哈希失败 %s: %s", filePath, err))
		return
	}

	_, err = file.Seek(0, 0)
	if err != nil {
		dp.Log(LogLevelError, fmt.Sprintf("无法重置文件指针 %s: %s", filePath, err))
		return
	}

	fileInfo := FileInfo{
		Path:    filePath,
		MD5Hash: md5Hash,
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}

	dp.mu.Lock()
	dp.fileMap[md5Hash] = append(dp.fileMap[md5Hash], fileInfo)
	dp.mu.Unlock()
}

func (dp *DuplProcessor) WaitForCompletion() {
	dp.wg.Wait()
	dp.Log(LogLevelInfo, "文件扫描完成")
}

func (dp *DuplProcessor) ProcessDuplicates() (int, map[string]string) {
	processedCount := 0
	resultsMap := make(map[string]string)
	
	totalGroups := 0
	for _, files := range dp.fileMap {
		if len(files) > 1 {
			totalGroups++
		}
	}
	
	if totalGroups == 0 {
		dp.Log(LogLevelInfo, "未发现重复文件")
		return 0, resultsMap
	}
	
	dp.Log(LogLevelInfo, fmt.Sprintf("找到 %d 组可能的重复文件", totalGroups))
	
	currentGroup := 0
	for md5Hash, files := range dp.fileMap {
		if len(files) <= 1 {
			continue
		}

		currentGroup++
		dp.updateProgress(fmt.Sprintf("处理重复文件组 (%d/%d)...", currentGroup, totalGroups))
		
		dp.Log(LogLevelInfo, fmt.Sprintf("\n发现可能的重复文件组 (MD5: %s):", md5Hash))
		for _, f := range files {
			dp.Log(LogLevelInfo, fmt.Sprintf("  - %s (%d 字节)", f.Path, f.Size))
		}

		duplicateGroups := dp.groupBySHA256(files)
		
		for _, group := range duplicateGroups {
			if len(group) <= 1 {
				continue
			}
			
			dp.Log(LogLevelInfo, fmt.Sprintf("\n确认重复文件组 (SHA256 验证通过):"))
			for _, f := range group {
				dp.Log(LogLevelInfo, fmt.Sprintf("  - %s", f.Path))
			}
			
			originalFile := dp.selectOriginalFile(group)
			dp.Log(LogLevelInfo, fmt.Sprintf("保留文件: %s", originalFile.Path))
			
			for i := 0; i < len(group); i++ {
				if group[i].Path == originalFile.Path {
					continue
				}
				
				duplicateFile := group[i]

				isLink, err := IsFileLink(duplicateFile.Path)
				if err != nil {
					dp.Log(LogLevelError, fmt.Sprintf("检查文件是否为链接时出错 %s: %s", duplicateFile.Path, err))
					continue
				}

				if isLink {
					dp.Log(LogLevelInfo, fmt.Sprintf("跳过已经是链接的文件: %s", duplicateFile.Path))
					dp.mu.Lock()
					dp.processedMap[duplicateFile.Path] = true
					dp.mu.Unlock()
					continue
				}
				
				dp.Log(LogLevelInfo, fmt.Sprintf("替换文件: %s", duplicateFile.Path))

				err = os.Remove(duplicateFile.Path)
				if err != nil {
					dp.Log(LogLevelError, fmt.Sprintf("删除文件 %s 失败: %s", duplicateFile.Path, err))
					continue
				}

				success := dp.createLink(originalFile.Path, duplicateFile.Path)
				
				if success {
					linkType := "软链接"
					if dp.IsHardLink {
						linkType = "硬链接"
					}
					dp.Log(LogLevelInfo, fmt.Sprintf("成功创建%s: %s -> %s", 
					           linkType, duplicateFile.Path, originalFile.Path))
					
					resultsMap[duplicateFile.Path] = originalFile.Path
					dp.mu.Lock()
					dp.processedMap[duplicateFile.Path] = true
					dp.mu.Unlock()
				}
			}
			
			dp.mu.Lock()
			dp.processedMap[originalFile.Path] = true
			dp.mu.Unlock()
			processedCount++
		}
	}
	
	return processedCount, resultsMap
}

func (dp *DuplProcessor) selectOriginalFile(files []FileInfo) FileInfo {
	bestFile := files[0]
	
	for i := 1; i < len(files); i++ {
		if files[i].ModTime.After(bestFile.ModTime) {
			bestFile = files[i]
		}
	}
	
	return bestFile
}

func (dp *DuplProcessor) createLink(originalPath, linkPath string) bool {
	maxRetries := 3
	var err error
	
	for retry := 0; retry < maxRetries; retry++ {
		if retry > 0 {
			dp.Log(LogLevelInfo, fmt.Sprintf("重试创建链接 (%d/%d)...", retry+1, maxRetries))
			time.Sleep(100 * time.Millisecond)
		}
		
		if dp.IsHardLink {
			err = os.Link(originalPath, linkPath)
		} else {
			absOriginal, pErr := filepath.Abs(originalPath)
			if pErr != nil {
				dp.Log(LogLevelError, fmt.Sprintf("获取绝对路径失败: %s", pErr))
				absOriginal = originalPath
			}
			err = os.Symlink(absOriginal, linkPath)
		}
		
		if err == nil {
			return true
		}
	}
	
	dp.Log(LogLevelError, fmt.Sprintf("创建链接失败 %s -> %s: %s", 
	       originalPath, linkPath, err))
	return false
}

func (dp *DuplProcessor) groupBySHA256(files []FileInfo) [][]FileInfo {
	sha256Map := make(map[string][]FileInfo)
	var wg sync.WaitGroup
	resultChan := make(chan struct{FileInfo; Hash string}, len(files))
	
	for _, file := range files {
		wg.Add(1)
		go func(f FileInfo) {
			defer wg.Done()
			
			sha256Hash, err := calculateSHA256(f.Path)
			if err != nil {
				dp.Log(LogLevelError, fmt.Sprintf("计算SHA256哈希失败 %s: %s", f.Path, err))
				return
			}
			
			resultChan <- struct{FileInfo; Hash string}{f, sha256Hash}
		}(file)
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	for result := range resultChan {
		f := result.FileInfo
		f.SHA256Hash = result.Hash
		sha256Map[f.SHA256Hash] = append(sha256Map[f.SHA256Hash], f)
	}
	
	var result [][]FileInfo
	for _, group := range sha256Map {
		result = append(result, group)
	}
	
	return result
}

func (dp *DuplProcessor) SaveState(filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	
	for path := range dp.processedMap {
		if dp.processedMap[path] {
			_, err := fmt.Fprintln(file, path)
			if err != nil {
				return err
			}
		}
	}
	
	return nil
}

func (dp *DuplProcessor) LoadState(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		path := scanner.Text()
		dp.processedMap[path] = true
	}
	
	return scanner.Err()
}

func CheckAdminPrivileges() bool {
	if runtime.GOOS != "windows" {
		return os.Geteuid() == 0
	}
	
	var token windows.Token
	process, err := windows.GetCurrentProcess()
	if err != nil {
		return false
	}
	
	err = windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token)
	if err != nil {
		return false
	}
	defer token.Close()
	
	var isElevated uint32
	var outLen uint32
	err = windows.GetTokenInformation(token, windows.TokenElevation, 
	                                 (*byte)(unsafe.Pointer(&isElevated)), 
	                                 uint32(unsafe.Sizeof(isElevated)), 
	                                 &outLen)
	
	if err != nil {
		return false
	}
	
	return isElevated != 0
}

func RequestAdminPrivileges() {
	if runtime.GOOS != "windows" {
		fmt.Println("在非Windows系统上无法自动请求管理员权限")
		return
	}
	
	verb := "runas"
	exe, _ := os.Executable()
	cwd, _ := os.Getwd()

	args := strings.Join(os.Args[1:], " ")
	hwnd := 0
	err := windows.ShellExecute(
		windows.Handle(hwnd), 
		utf16PtrFromString(verb), 
		utf16PtrFromString(exe), 
		utf16PtrFromString(args), 
		utf16PtrFromString(cwd), 
		windows.SW_NORMAL,
	)
	
	if err != nil {
		fmt.Printf("无法以管理员权限启动: %v\n", err)
	}
}

func utf16PtrFromString(s string) *uint16 {
	ptr, _ := windows.UTF16PtrFromString(s)
	return ptr
}

func calculateMD5(file *os.File) (string, error) {
	hash := md5.New()
	buf := make([]byte, 4*1024*1024)
	if _, err := io.CopyBuffer(hash, file, buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func calculateSHA256(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	buf := make([]byte, 4*1024*1024)
	if _, err := io.CopyBuffer(hash, file, buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func IsFileLink(filePath string) (bool, error) {
	fileInfo, err := os.Lstat(filePath)
	if err != nil {
		return false, err
	}
	
	if fileInfo.Mode()&os.ModeSymlink != 0 {
		return true, nil
	}
	
	if runtime.GOOS == "windows" {
		handle, err := windows.CreateFile(
			utf16PtrFromString(filePath),
			windows.GENERIC_READ,
			windows.FILE_SHARE_READ,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if err != nil {
			return false, fmt.Errorf("打开文件失败: %w", err)
		}
		defer windows.CloseHandle(handle)
		
		var fileInfo windows.ByHandleFileInformation
		err = windows.GetFileInformationByHandle(handle, &fileInfo)
		if err != nil {
			return false, fmt.Errorf("获取文件信息失败: %w", err)
		}
		
		return fileInfo.NumberOfLinks > 1, nil
	}
	
	return false, nil
}

func TerminateWeChatProcess() error {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("taskkill", "/F", "/IM", "WeChat.exe")
		err := cmd.Run()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 128 {
				return nil
			}
			return fmt.Errorf("终止 WeChat 进程失败: %w", err)
		}
		return nil
	}
	
	return fmt.Errorf("不支持的操作系统: %s", runtime.GOOS)
}

func (dp *DuplProcessor) HasProcessedFiles() bool {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	
	return len(dp.processedMap) > 0
}

func (dp *DuplProcessor) ResetProcessedFiles() {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	
	dp.processedMap = make(map[string]bool)
	dp.fileMap = make(map[string][]FileInfo)
	dp.Log(LogLevelInfo, "已重置处理状态，将重新扫描所有文件")
}