package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"encoding/json"
	"syscall"
	"os/signal"
	"os/exec"
	"runtime"

	"github.com/schollz/progressbar/v3"
	"github.com/fatih/color"

	"duplfiles2link/pkg/duplfiles2link"
)

const (
	stateFile = "duplfiles2link_state.dat"
	logFile   = "duplfiles2link.log"
)

func main() {
	logOutput, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("无法打开日志文件: %s\n", err)
		return
	}
	defer logOutput.Close()

	infoColor := color.New(color.FgCyan)
	errorColor := color.New(color.FgRed, color.Bold)
	titleColor := color.New(color.FgGreen, color.Bold)
	highlightColor := color.New(color.FgYellow)
	bannerColor := color.New(color.FgHiBlue, color.Bold)

	// 日志函数只写入文件，不输出到控制台
	logToFile := func(message string) {
		timestamp := time.Now().Format("2006-01-02 15:04:05")
		fmt.Fprintf(logOutput, "[%s] %s\n", timestamp, message)
	}

	// 日志助手函数，既写入文件又显示彩色输出
	logInfo := func(message string) {
		logToFile(message)
		infoColor.Println(message)
	}

	logError := func(message string) {
		logToFile(message)
		errorColor.Println(message)
	}

	logTitle := func(message string) {
		logToFile(message)
		titleColor.Println(message)
	}

	logHighlight := func(message string) {
		logToFile(message)
		highlightColor.Println(message)
	}

	clearScreen()
	showBanner(bannerColor)
	
	logTitle("微信文件去重程序 - 开始执行")
	logInfo(duplfiles2link.GetVersion())
	logTitle("===============================")

	isAdmin := duplfiles2link.CheckAdminPrivileges()
	if !isAdmin {
		logError("警告: 程序没有管理员权限，将自动请求提升权限")
		logInfo("正在请求管理员权限...")
		duplfiles2link.RequestAdminPrivileges()
		logInfo("已启动新进程，本进程将退出")
		return
	}

	wechatPath := findWeChatPath()
	if wechatPath == "" {
		logInfo("未能自动找到微信文件夹，请手动输入路径:")
		reader := bufio.NewReader(os.Stdin)
		wechatPath, _ = reader.ReadString('\n')
		wechatPath = strings.TrimSpace(wechatPath)
		
		if wechatPath == "" {
			logError("未提供路径，退出程序")
			return
		}
	}

	logInfo(fmt.Sprintf("使用微信文件路径: %s", wechatPath))

	highlightColor.Print("请选择链接类型 [h-硬链接/s-软链接] (默认为硬链接): ")
	reader := bufio.NewReader(os.Stdin)
	linkType, _ := reader.ReadString('\n')
	linkType = strings.TrimSpace(linkType)
	
	isHardLink := linkType != "s"
	if isHardLink {
		logInfo("使用硬链接模式")
	} else {
		logInfo("使用软链接模式")
	}


	processor := duplfiles2link.NewProcessor(isHardLink)
	var bar *progressbar.ProgressBar
	lastUpdate := time.Now()
	updateInterval := 1000 * time.Millisecond
	
	processor.SetProgressCallback(func(current, total int, message string) {
		if bar == nil {
			bar = progressbar.NewOptions(total,
				progressbar.OptionSetDescription(message),
				progressbar.OptionShowCount(),
				progressbar.OptionShowIts(),
				progressbar.OptionSetWidth(50),
				progressbar.OptionThrottle(updateInterval),
				progressbar.OptionSpinnerType(14),
				progressbar.OptionFullWidth(),
				progressbar.OptionSetRenderBlankState(true),
			)
		} else if int(bar.GetMax()) != total {
			_ = bar.Finish()
			bar = progressbar.NewOptions(total,
				progressbar.OptionSetDescription(message),
				progressbar.OptionShowCount(),
				progressbar.OptionShowIts(),
				progressbar.OptionSetWidth(50),
				progressbar.OptionThrottle(updateInterval),
				progressbar.OptionSpinnerType(14),
				progressbar.OptionFullWidth(),
				progressbar.OptionSetRenderBlankState(true),
			)
		}
		
		if bar.String() != message {
			bar.Describe(message)
		}
		
		if time.Since(lastUpdate) > updateInterval || current == total {
			_ = bar.Set(current)
			lastUpdate = time.Now()
		}
	})
	
	processor.SetLogCallback(func(level duplfiles2link.LogLevel, message string) {
		timestamp := time.Now().Format("2006-01-02 15:04:05")
		fmt.Fprintf(logOutput, "[%s] [%s] %s\n", timestamp, level, message)
		
		switch level {
		case duplfiles2link.LogLevelInfo:
			if strings.HasPrefix(message, "\n") {
				fmt.Println()
				message = strings.TrimPrefix(message, "\n")
			}
			infoColor.Printf("[INFO] %s\n", message)
		case duplfiles2link.LogLevelError:
			errorColor.Printf("[ERROR] %s\n", message)
		case duplfiles2link.LogLevelDebug:
			// Debug级别的日志默认不显示在控制台
		}
	})
	
	err = processor.LoadState(stateFile)
	if err != nil {
		logError(fmt.Sprintf("加载状态文件失败: %s", err))
	} else {
		logInfo("已加载之前的处理状态")
		
		if processor.HasProcessedFiles() {
			logHighlight("是否重新扫描所有文件? [y-是/n-否] (默认: 否): ")
			reader := bufio.NewReader(os.Stdin)
			choice, _ := reader.ReadString('\n')
			choice = strings.TrimSpace(choice)
			
			if strings.ToLower(choice) == "y" || strings.ToLower(choice) == "yes" {
				logInfo("用户选择重新扫描所有文件")
				processor.ResetProcessedFiles()
			} else {
				logInfo("继续使用已有状态，将跳过已处理的文件")
			}
		}
	}
	
	setupSignalHandler(processor)

	logTitle(fmt.Sprintf("开始处理路径: %s", wechatPath))
	
	logInfo("正在结束 WeChat 进程...")
	err = duplfiles2link.TerminateWeChatProcess()
	if err != nil {
		logError(fmt.Sprintf("结束 WeChat 进程时出错: %s", err))
	} else {
		logInfo("已结束 WeChat 进程，等待 3 秒...")
		time.Sleep(3 * time.Second)
	}
	
	err = processor.ProcessPath(wechatPath)
	if err != nil {
		logError(fmt.Sprintf("处理路径时出错: %s", err))
		return
	}

	logInfo("等待文件扫描完成...")
	processor.WaitForCompletion()

	logTitle("\n开始处理重复文件...")
	count, results := processor.ProcessDuplicates()
	
	saveResults(results)
	
	if processor.HasProcessedFiles() {
		err = processor.SaveState(stateFile)
		if err != nil {
			logError(fmt.Sprintf("保存状态失败: %s", err))
		} else {
			logInfo("状态已保存，可以在下次运行时继续")
		}
	} else {
		logInfo("没有处理状态需要保存")
	}

	logTitle(fmt.Sprintf("\n处理完成! 共处理了 %d 组重复文件", count))
	logInfo("结果已保存到 duplfiles2link_results.json")
	logInfo("文件处理完成，现在可以重新启动 WeChat...")
	logHighlight("按 Enter 键退出...")
	bufio.NewReader(os.Stdin).ReadBytes('\n')
}

func findWeChatPath() string {
	userProfile := os.Getenv("USERPROFILE")
	if userProfile == "" {
		return ""
	}
	
	wechatPath := filepath.Join(userProfile, "Documents", "WeChat Files")
	_, err := os.Stat(wechatPath)
	if err == nil {
		return wechatPath
	}
	
	documents := filepath.Join(userProfile, "Documents")
	entries, err := os.ReadDir(documents)
	if err != nil {
		return ""
	}
	
	for _, entry := range entries {
		if entry.IsDir() && strings.Contains(strings.ToLower(entry.Name()), "wechat") {
			path := filepath.Join(documents, entry.Name())
			return path
		}
	}
	
	return ""
}

func setupSignalHandler(processor *duplfiles2link.DuplProcessor) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	
	go func() {
		<-c
		fmt.Println("\n接收到中断信号，正在保存状态...")

		if processor.HasProcessedFiles() {
			err := processor.SaveState(stateFile)
			if err != nil {
				fmt.Printf("保存状态失败: %s\n", err)
			} else {
				fmt.Println("状态已保存，可以在下次运行时继续")
			}
		} else {
			fmt.Println("没有处理状态需要保存")
		}
		
		os.Exit(0)
	}()
}

func saveResults(results map[string]string) {
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		fmt.Printf("保存结果失败: %s\n", err)
		return
	}
	
	err = os.WriteFile("duplfiles2link_results.json", data, 0644)
	if err != nil {
		fmt.Printf("写入结果文件失败: %s\n", err)
	}
}

func clearScreen() {
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("cmd", "/c", "cls")
		cmd.Stdout = os.Stdout
		cmd.Run()
	default:
		fmt.Print("\033[H\033[2J")
	}
}

func showBanner(bannerColor *color.Color) {
	banner := []string{
		"██████╗ ██╗   ██╗██████╗ ██╗     ███████╗██╗██╗     ███████╗███████╗██████╗ ██╗     ██╗███╗   ██╗██╗  ██╗",
		"██╔══██╗██║   ██║██╔══██╗██║     ██╔════╝██║██║     ██╔════╝██╔════╝██╔══██╗██║     ██║████╗  ██║██║ ██╔╝",
		"██║  ██║██║   ██║██████╔╝██║     █████╗  ██║██║     █████╗  ███████╗██████╔╝██║     ██║██╔██╗ ██║█████╔╝ ",
		"██║  ██║██║   ██║██╔═══╝ ██║     ██╔══╝  ██║██║     ██╔══╝  ╚════██║██╔══██╗██║     ██║██║╚██╗██║██╔═██╗ ",
		"██████╔╝╚██████╔╝██║     ███████╗██║     ██║███████╗███████╗███████║██║  ██║███████╗██║██║ ╚████║██║  ██╗",
		"╚═════╝  ╚═════╝ ╚═╝     ╚══════╝╚═╝     ╚═╝╚══════╝╚══════╝╚══════╝╚═╝  ╚═╝╚══════╝╚═╝╚═╝  ╚═══╝╚═╝  ╚═╝",
	}
	for _, line := range banner {
		bannerColor.Println(line)
	}
	fmt.Println()
} 