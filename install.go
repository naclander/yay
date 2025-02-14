package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	alpm "github.com/Jguer/go-alpm"
	"github.com/Jguer/yay/v9/pkg/completion"
	"github.com/Jguer/yay/v9/pkg/intrange"
	"github.com/Jguer/yay/v9/pkg/multierror"
	"github.com/Jguer/yay/v9/pkg/stringset"
	gosrc "github.com/Morganamilo/go-srcinfo"
)

func asdeps(parser *arguments, pkgs []string) error {
	if len(pkgs) == 0 {
		return nil
	}

	parser = parser.copyGlobal()
	_ = parser.addArg("D", "asdeps")
	parser.addTarget(pkgs...)
	_, stderr, err := capture(passToPacman(parser))
	if err != nil {
		return fmt.Errorf("%s%s", stderr, err)
	}

	return nil
}

func asexp(parser *arguments, pkgs []string) error {
	if len(pkgs) == 0 {
		return nil
	}

	parser = parser.copyGlobal()
	_ = parser.addArg("D", "asexplicit")
	parser.addTarget(pkgs...)
	_, stderr, err := capture(passToPacman(parser))
	if err != nil {
		return fmt.Errorf("%s%s", stderr, err)
	}

	return nil
}

// Install handles package installs
func install(parser *arguments) (err error) {
	var incompatible stringset.StringSet
	var do *depOrder

	var aurUp upSlice
	var repoUp upSlice

	var srcinfos map[string]*gosrc.Srcinfo

	warnings := &aurWarnings{}

	if mode == modeAny || mode == modeRepo {
		if config.CombinedUpgrade {
			if parser.existsArg("y", "refresh") {
				err = earlyRefresh(parser)
				if err != nil {
					return fmt.Errorf("Error refreshing databases")
				}
			}
		} else if parser.existsArg("y", "refresh") || parser.existsArg("u", "sysupgrade") || len(parser.targets) > 0 {
			err = earlyPacmanCall(parser)
			if err != nil {
				return err
			}
		}
	}

	//we may have done -Sy, our handle now has an old
	//database.
	err = initAlpmHandle()
	if err != nil {
		return err
	}

	_, _, localNames, remoteNames, err := filterPackages()
	if err != nil {
		return err
	}

	remoteNamesCache := stringset.FromSlice(remoteNames)
	localNamesCache := stringset.FromSlice(localNames)

	requestTargets := parser.copy().targets

	//create the arguments to pass for the repo install
	arguments := parser.copy()
	arguments.delArg("asdeps", "asdep")
	arguments.delArg("asexplicit", "asexp")
	arguments.op = "S"
	arguments.clearTargets()

	if mode == modeAUR {
		arguments.delArg("u", "sysupgrade")
	}

	//if we are doing -u also request all packages needing update
	if parser.existsArg("u", "sysupgrade") {
		aurUp, repoUp, err = upList(warnings)
		if err != nil {
			return err
		}

		warnings.print()

		ignore, aurUp, err := upgradePkgs(aurUp, repoUp)
		if err != nil {
			return err
		}

		for _, up := range repoUp {
			if !ignore.Get(up.Name) {
				requestTargets = append(requestTargets, up.Name)
				parser.addTarget(up.Name)
			}
		}

		for up := range aurUp {
			requestTargets = append(requestTargets, "aur/"+up)
			parser.addTarget("aur/" + up)
		}

		value, _, exists := cmdArgs.getArg("ignore")

		if len(ignore) > 0 {
			ignoreStr := strings.Join(ignore.ToSlice(), ",")
			if exists {
				ignoreStr += "," + value
			}
			arguments.options["ignore"] = ignoreStr
		}
	}

	targets := stringset.FromSlice(parser.targets)

	dp, err := getDepPool(requestTargets, warnings)
	if err != nil {
		return err
	}

	err = dp.CheckMissing()
	if err != nil {
		return err
	}

	if len(dp.Aur) == 0 {
		if !config.CombinedUpgrade {
			if parser.existsArg("u", "sysupgrade") {
				fmt.Println(" there is nothing to do")
			}
			return nil
		}

		parser.op = "S"
		parser.delArg("y", "refresh")
		parser.options["ignore"] = arguments.options["ignore"]
		return show(passToPacman(parser))
	}

	if len(dp.Aur) > 0 && os.Geteuid() == 0 {
		return fmt.Errorf(bold(red(arrow)) + " Refusing to install AUR Packages as root, Aborting.")
	}

	conflicts, err := dp.CheckConflicts()
	if err != nil {
		return err
	}

	do = getDepOrder(dp)
	if err != nil {
		return err
	}

	for _, pkg := range do.Repo {
		arguments.addTarget(pkg.DB().Name() + "/" + pkg.Name())
	}

	for _, pkg := range dp.Groups {
		arguments.addTarget(pkg)
	}

	if len(do.Aur) == 0 && len(arguments.targets) == 0 && (!parser.existsArg("u", "sysupgrade") || mode == modeAUR) {
		fmt.Println(" there is nothing to do")
		return nil
	}

	do.Print()
	fmt.Println()

	if config.CleanAfter {
		defer cleanAfter(do.Aur)
	}

	if do.HasMake() {
		switch config.RemoveMake {
		case "yes":
			defer func() {
				err = removeMake(do)
			}()

		case "no":
			break
		default:
			if continueTask("Remove make dependencies after install?", false) {
				defer func() {
					err = removeMake(do)
				}()
			}
		}
	}

	if config.CleanMenu {
		if anyExistInCache(do.Aur) {
			askClean := pkgbuildNumberMenu(do.Aur, remoteNamesCache)
			toClean, err := cleanNumberMenu(do.Aur, remoteNamesCache, askClean)
			if err != nil {
				return err
			}

			cleanBuilds(toClean)
		}
	}

	toSkip := pkgbuildsToSkip(do.Aur, targets)
	cloned, err := downloadPkgbuilds(do.Aur, toSkip, config.BuildDir)
	if err != nil {
		return err
	}

	var toDiff []Base
	var toEdit []Base

	if config.DiffMenu {
		pkgbuildNumberMenu(do.Aur, remoteNamesCache)
		toDiff, err = diffNumberMenu(do.Aur, remoteNamesCache)
		if err != nil {
			return err
		}

		if len(toDiff) > 0 {
			// TODO: PKGBUILD diffs should not return in case of err. Just print and continue
			err = showPkgbuildDiffs(toDiff, cloned)
			if err != nil {
				return err
			}
		}
	}

	if len(toDiff) > 0 {
		oldValue := config.NoConfirm
		config.NoConfirm = false
		fmt.Println()
		if !continueTask(bold(green("Proceed with install?")), true) {
			return fmt.Errorf("Aborting due to user")
		}
		err = updatePkgbuildSeenRef(toDiff, cloned)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
		}

		config.NoConfirm = oldValue
	}

	err = mergePkgbuilds(do.Aur)
	if err != nil {
		return err
	}

	srcinfos, err = parseSrcinfoFiles(do.Aur, true)
	if err != nil {
		return err
	}

	if config.EditMenu {
		pkgbuildNumberMenu(do.Aur, remoteNamesCache)
		toEdit, err = editNumberMenu(do.Aur, remoteNamesCache)
		if err != nil {
			return err
		}

		if len(toEdit) > 0 {
			err = editPkgbuilds(toEdit, srcinfos)
			if err != nil {
				return err
			}
		}
	}

	if len(toEdit) > 0 {
		oldValue := config.NoConfirm
		config.NoConfirm = false
		fmt.Println()
		if !continueTask(bold(green("Proceed with install?")), true) {
			return fmt.Errorf("Aborting due to user")
		}
		config.NoConfirm = oldValue
	}

	incompatible, err = getIncompatible(do.Aur, srcinfos)
	if err != nil {
		return err
	}

	if config.PGPFetch {
		err = checkPgpKeys(do.Aur, srcinfos)
		if err != nil {
			return err
		}
	}

	if !config.CombinedUpgrade {
		arguments.delArg("u", "sysupgrade")
	}

	if len(arguments.targets) > 0 || arguments.existsArg("u") {
		err := show(passToPacman(arguments))
		if err != nil {
			return fmt.Errorf("Error installing repo packages")
		}

		deps := make([]string, 0)
		exp := make([]string, 0)

		for _, pkg := range do.Repo {
			if !dp.Explicit.Get(pkg.Name()) && !localNamesCache.Get(pkg.Name()) && !remoteNamesCache.Get(pkg.Name()) {
				deps = append(deps, pkg.Name())
				continue
			}

			if parser.existsArg("asdeps", "asdep") && dp.Explicit.Get(pkg.Name()) {
				deps = append(deps, pkg.Name())
			} else if parser.existsArg("asexp", "asexplicit") && dp.Explicit.Get(pkg.Name()) {
				exp = append(exp, pkg.Name())
			}
		}

		if err = asdeps(parser, deps); err != nil {
			return err
		}
		if err = asexp(parser, exp); err != nil {
			return err
		}
	}

	go exitOnError(completion.Update(alpmHandle, config.AURURL, cacheHome, config.CompletionInterval, false))

	err = downloadPkgbuildsSources(do.Aur, incompatible)
	if err != nil {
		return err
	}

	err = buildInstallPkgbuilds(dp, do, srcinfos, parser, incompatible, conflicts)
	if err != nil {
		return err
	}

	return nil
}

func removeMake(do *depOrder) error {
	removeArguments := makeArguments()
	err := removeArguments.addArg("R", "u")
	if err != nil {
		return err
	}

	for _, pkg := range do.getMake() {
		removeArguments.addTarget(pkg)
	}

	oldValue := config.NoConfirm
	config.NoConfirm = true
	err = show(passToPacman(removeArguments))
	config.NoConfirm = oldValue

	return err
}

func inRepos(syncDB alpm.DBList, pkg string) bool {
	target := toTarget(pkg)

	if target.DB == "aur" {
		return false
	} else if target.DB != "" {
		return true
	}

	previousHideMenus := hideMenus
	hideMenus = false
	_, err := syncDB.FindSatisfier(target.DepString())
	hideMenus = previousHideMenus
	if err == nil {
		return true
	}

	return !syncDB.FindGroupPkgs(target.Name).Empty()
}

func earlyPacmanCall(parser *arguments) error {
	arguments := parser.copy()
	arguments.op = "S"
	targets := parser.targets
	parser.clearTargets()
	arguments.clearTargets()

	syncDB, err := alpmHandle.SyncDBs()
	if err != nil {
		return err
	}

	if mode == modeRepo {
		arguments.targets = targets
	} else {
		//separate aur and repo targets
		for _, target := range targets {
			if inRepos(syncDB, target) {
				arguments.addTarget(target)
			} else {
				parser.addTarget(target)
			}
		}
	}

	if parser.existsArg("y", "refresh") || parser.existsArg("u", "sysupgrade") || len(arguments.targets) > 0 {
		err = show(passToPacman(arguments))
		if err != nil {
			return fmt.Errorf("Error installing repo packages")
		}
	}

	return nil
}

func earlyRefresh(parser *arguments) error {
	arguments := parser.copy()
	parser.delArg("y", "refresh")
	arguments.delArg("u", "sysupgrade")
	arguments.delArg("s", "search")
	arguments.delArg("i", "info")
	arguments.delArg("l", "list")
	arguments.clearTargets()
	return show(passToPacman(arguments))
}

func getIncompatible(bases []Base, srcinfos map[string]*gosrc.Srcinfo) (stringset.StringSet, error) {
	incompatible := make(stringset.StringSet)
	basesMap := make(map[string]Base)
	alpmArch, err := alpmHandle.Arch()
	if err != nil {
		return nil, err
	}

nextpkg:
	for _, base := range bases {
		for _, arch := range srcinfos[base.Pkgbase()].Arch {
			if arch == "any" || arch == alpmArch {
				continue nextpkg
			}
		}

		incompatible.Set(base.Pkgbase())
		basesMap[base.Pkgbase()] = base
	}

	if len(incompatible) > 0 {
		fmt.Println()
		fmt.Print(bold(yellow(arrow)) + " The following packages are not compatible with your architecture:")
		for pkg := range incompatible {
			fmt.Print("  " + cyan(basesMap[pkg].String()))
		}

		fmt.Println()

		if !continueTask("Try to build them anyway?", true) {
			return nil, fmt.Errorf("Aborting due to user")
		}
	}

	return incompatible, nil
}

func parsePackageList(dir string) (map[string]string, string, error) {
	stdout, stderr, err := capture(passToMakepkg(dir, "--packagelist"))

	if err != nil {
		return nil, "", fmt.Errorf("%s%s", stderr, err)
	}

	var version string
	lines := strings.Split(stdout, "\n")
	pkgdests := make(map[string]string)

	for _, line := range lines {
		if line == "" {
			continue
		}

		fileName := filepath.Base(line)
		split := strings.Split(fileName, "-")

		if len(split) < 4 {
			return nil, "", fmt.Errorf("Can not find package name : %s", split)
		}

		// pkgname-pkgver-pkgrel-arch.pkgext
		// This assumes 3 dashes after the pkgname, Will cause an error
		// if the PKGEXT contains a dash. Please no one do that.
		pkgname := strings.Join(split[:len(split)-3], "-")
		version = strings.Join(split[len(split)-3:len(split)-1], "-")
		pkgdests[pkgname] = line
	}

	return pkgdests, version, nil
}

func anyExistInCache(bases []Base) bool {
	for _, base := range bases {
		pkg := base.Pkgbase()
		dir := filepath.Join(config.BuildDir, pkg)

		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			return true
		}
	}

	return false
}

func pkgbuildNumberMenu(bases []Base, installed stringset.StringSet) bool {
	toPrint := ""
	askClean := false

	for n, base := range bases {
		pkg := base.Pkgbase()
		dir := filepath.Join(config.BuildDir, pkg)

		toPrint += fmt.Sprintf(magenta("%3d")+" %-40s", len(bases)-n,
			bold(base.String()))

		anyInstalled := false
		for _, b := range base {
			anyInstalled = anyInstalled || installed.Get(b.Name)
		}

		if anyInstalled {
			toPrint += bold(green(" (Installed)"))
		}

		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			toPrint += bold(green(" (Build Files Exist)"))
			askClean = true
		}

		toPrint += "\n"
	}

	fmt.Print(toPrint)

	return askClean
}

func cleanNumberMenu(bases []Base, installed stringset.StringSet, hasClean bool) ([]Base, error) {
	toClean := make([]Base, 0)

	if !hasClean {
		return toClean, nil
	}

	fmt.Println(bold(green(arrow + " Packages to cleanBuild?")))
	fmt.Println(bold(green(arrow) + cyan(" [N]one ") + "[A]ll [Ab]ort [I]nstalled [No]tInstalled or (1 2 3, 1-3, ^4)"))
	fmt.Print(bold(green(arrow + " ")))
	cleanInput, err := getInput(config.AnswerClean)
	if err != nil {
		return nil, err
	}

	cInclude, cExclude, cOtherInclude, cOtherExclude := intrange.ParseNumberMenu(cleanInput)
	cIsInclude := len(cExclude) == 0 && len(cOtherExclude) == 0

	if cOtherInclude.Get("abort") || cOtherInclude.Get("ab") {
		return nil, fmt.Errorf("Aborting due to user")
	}

	if !cOtherInclude.Get("n") && !cOtherInclude.Get("none") {
		for i, base := range bases {
			pkg := base.Pkgbase()
			anyInstalled := false
			for _, b := range base {
				anyInstalled = anyInstalled || installed.Get(b.Name)
			}

			dir := filepath.Join(config.BuildDir, pkg)
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				continue
			}

			if !cIsInclude && cExclude.Get(len(bases)-i) {
				continue
			}

			if anyInstalled && (cOtherInclude.Get("i") || cOtherInclude.Get("installed")) {
				toClean = append(toClean, base)
				continue
			}

			if !anyInstalled && (cOtherInclude.Get("no") || cOtherInclude.Get("notinstalled")) {
				toClean = append(toClean, base)
				continue
			}

			if cOtherInclude.Get("a") || cOtherInclude.Get("all") {
				toClean = append(toClean, base)
				continue
			}

			if cIsInclude && (cInclude.Get(len(bases)-i) || cOtherInclude.Get(pkg)) {
				toClean = append(toClean, base)
				continue
			}

			if !cIsInclude && (!cExclude.Get(len(bases)-i) && !cOtherExclude.Get(pkg)) {
				toClean = append(toClean, base)
				continue
			}
		}
	}

	return toClean, nil
}

func editNumberMenu(bases []Base, installed stringset.StringSet) ([]Base, error) {
	return editDiffNumberMenu(bases, installed, false)
}

func diffNumberMenu(bases []Base, installed stringset.StringSet) ([]Base, error) {
	return editDiffNumberMenu(bases, installed, true)
}

func editDiffNumberMenu(bases []Base, installed stringset.StringSet, diff bool) ([]Base, error) {
	toEdit := make([]Base, 0)
	var editInput string
	var err error

	if diff {
		fmt.Println(bold(green(arrow + " Diffs to show?")))
		fmt.Println(bold(green(arrow) + cyan(" [N]one ") + "[A]ll [Ab]ort [I]nstalled [No]tInstalled or (1 2 3, 1-3, ^4)"))
		fmt.Print(bold(green(arrow + " ")))
		editInput, err = getInput(config.AnswerDiff)
		if err != nil {
			return nil, err
		}
	} else {
		fmt.Println(bold(green(arrow + " PKGBUILDs to edit?")))
		fmt.Println(bold(green(arrow) + cyan(" [N]one ") + "[A]ll [Ab]ort [I]nstalled [No]tInstalled or (1 2 3, 1-3, ^4)"))
		fmt.Print(bold(green(arrow + " ")))
		editInput, err = getInput(config.AnswerEdit)
		if err != nil {
			return nil, err
		}
	}

	eInclude, eExclude, eOtherInclude, eOtherExclude := intrange.ParseNumberMenu(editInput)
	eIsInclude := len(eExclude) == 0 && len(eOtherExclude) == 0

	if eOtherInclude.Get("abort") || eOtherInclude.Get("ab") {
		return nil, fmt.Errorf("Aborting due to user")
	}

	if !eOtherInclude.Get("n") && !eOtherInclude.Get("none") {
		for i, base := range bases {
			pkg := base.Pkgbase()
			anyInstalled := false
			for _, b := range base {
				anyInstalled = anyInstalled || installed.Get(b.Name)
			}

			if !eIsInclude && eExclude.Get(len(bases)-i) {
				continue
			}

			if anyInstalled && (eOtherInclude.Get("i") || eOtherInclude.Get("installed")) {
				toEdit = append(toEdit, base)
				continue
			}

			if !anyInstalled && (eOtherInclude.Get("no") || eOtherInclude.Get("notinstalled")) {
				toEdit = append(toEdit, base)
				continue
			}

			if eOtherInclude.Get("a") || eOtherInclude.Get("all") {
				toEdit = append(toEdit, base)
				continue
			}

			if eIsInclude && (eInclude.Get(len(bases)-i) || eOtherInclude.Get(pkg)) {
				toEdit = append(toEdit, base)
			}

			if !eIsInclude && (!eExclude.Get(len(bases)-i) && !eOtherExclude.Get(pkg)) {
				toEdit = append(toEdit, base)
			}
		}
	}

	return toEdit, nil
}

func updatePkgbuildSeenRef(bases []Base, cloned stringset.StringSet) error {
	var errMulti multierror.MultiError
	for _, base := range bases {
		pkg := base.Pkgbase()
		dir := filepath.Join(config.BuildDir, pkg)
		if shouldUseGit(dir) {
			err := gitUpdateSeenRef(config.BuildDir, pkg)
			if err != nil {
				errMulti.Add(err)
			}
		}
	}
	return errMulti.Return()
}

func showPkgbuildDiffs(bases []Base, cloned stringset.StringSet) error {
	var errMulti multierror.MultiError
	for _, base := range bases {
		pkg := base.Pkgbase()
		dir := filepath.Join(config.BuildDir, pkg)
		if shouldUseGit(dir) {
			start, err := getLastSeenHash(config.BuildDir, pkg)
			if err != nil {
				errMulti.Add(err)
				continue
			}

			if cloned.Get(pkg) {
				start = gitEmptyTree
			} else {
				hasDiff, err := gitHasDiff(config.BuildDir, pkg)
				if err != nil {
					errMulti.Add(err)
					continue
				}

				if !hasDiff {
					fmt.Printf("%s %s: %s\n", bold(yellow(arrow)), cyan(base.String()), bold("No changes -- skipping"))
					continue
				}
			}

			args := []string{"diff", start + "..HEAD@{upstream}", "--src-prefix", dir + "/", "--dst-prefix", dir + "/", "--", ".", ":(exclude).SRCINFO"}
			if useColor {
				args = append(args, "--color=always")
			} else {
				args = append(args, "--color=never")
			}
			err = show(passToGit(dir, args...))
			if err != nil {
				errMulti.Add(err)
				continue
			}
		} else {
			args := []string{"diff"}
			if useColor {
				args = append(args, "--color=always")
			} else {
				args = append(args, "--color=never")
			}
			args = append(args, "--no-index", "/var/empty", dir)
			// git always returns 1. why? I have no idea
			err := show(passToGit(dir, args...))
			if err != nil {
				errMulti.Add(err)
				continue
			}
		}
	}

	return errMulti.Return()
}

func editPkgbuilds(bases []Base, srcinfos map[string]*gosrc.Srcinfo) error {
	pkgbuilds := make([]string, 0, len(bases))
	for _, base := range bases {
		pkg := base.Pkgbase()
		dir := filepath.Join(config.BuildDir, pkg)
		pkgbuilds = append(pkgbuilds, filepath.Join(dir, "PKGBUILD"))

		for _, splitPkg := range srcinfos[pkg].SplitPackages() {
			if splitPkg.Install != "" {
				pkgbuilds = append(pkgbuilds, filepath.Join(dir, splitPkg.Install))
			}
		}
	}

	if len(pkgbuilds) > 0 {
		editor, editorArgs := editor()
		editorArgs = append(editorArgs, pkgbuilds...)
		editcmd := exec.Command(editor, editorArgs...)
		editcmd.Stdin, editcmd.Stdout, editcmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		err := editcmd.Run()
		if err != nil {
			return fmt.Errorf("Editor did not exit successfully, Aborting: %s", err)
		}
	}

	return nil
}

func parseSrcinfoFiles(bases []Base, errIsFatal bool) (map[string]*gosrc.Srcinfo, error) {
	srcinfos := make(map[string]*gosrc.Srcinfo)
	for k, base := range bases {
		pkg := base.Pkgbase()
		dir := filepath.Join(config.BuildDir, pkg)

		str := bold(cyan("::") + " Parsing SRCINFO (%d/%d): %s\n")
		fmt.Printf(str, k+1, len(bases), cyan(base.String()))

		pkgbuild, err := gosrc.ParseFile(filepath.Join(dir, ".SRCINFO"))
		if err != nil {
			if !errIsFatal {
				fmt.Fprintf(os.Stderr, "failed to parse %s -- skipping: %s\n", base.String(), err)
				continue
			}
			return nil, fmt.Errorf("failed to parse %s: %s", base.String(), err)
		}

		srcinfos[pkg] = pkgbuild
	}

	return srcinfos, nil
}

func pkgbuildsToSkip(bases []Base, targets stringset.StringSet) stringset.StringSet {
	toSkip := make(stringset.StringSet)

	for _, base := range bases {
		isTarget := false
		for _, pkg := range base {
			isTarget = isTarget || targets.Get(pkg.Name)
		}

		if (config.ReDownload == "yes" && isTarget) || config.ReDownload == "all" {
			continue
		}

		dir := filepath.Join(config.BuildDir, base.Pkgbase(), ".SRCINFO")
		pkgbuild, err := gosrc.ParseFile(dir)

		if err == nil {
			if alpm.VerCmp(pkgbuild.Version(), base.Version()) >= 0 {
				toSkip.Set(base.Pkgbase())
			}
		}
	}

	return toSkip
}

func mergePkgbuilds(bases []Base) error {
	for _, base := range bases {
		if shouldUseGit(filepath.Join(config.BuildDir, base.Pkgbase())) {
			err := gitMerge(config.BuildDir, base.Pkgbase())
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func downloadPkgbuilds(bases []Base, toSkip stringset.StringSet, buildDir string) (stringset.StringSet, error) {
	cloned := make(stringset.StringSet)
	downloaded := 0
	var wg sync.WaitGroup
	var mux sync.Mutex
	var errs multierror.MultiError

	download := func(k int, base Base) {
		defer wg.Done()
		pkg := base.Pkgbase()

		if toSkip.Get(pkg) {
			mux.Lock()
			downloaded++
			str := bold(cyan("::") + " PKGBUILD up to date, Skipping (%d/%d): %s\n")
			fmt.Printf(str, downloaded, len(bases), cyan(base.String()))
			mux.Unlock()
			return
		}

		if shouldUseGit(filepath.Join(config.BuildDir, pkg)) {
			clone, err := gitDownload(config.AURURL+"/"+pkg+".git", buildDir, pkg)
			if err != nil {
				errs.Add(err)
				return
			}
			if clone {
				mux.Lock()
				cloned.Set(pkg)
				mux.Unlock()
			}
		} else {
			err := downloadAndUnpack(config.AURURL+base.URLPath(), buildDir)
			if err != nil {
				errs.Add(err)
				return
			}
		}

		mux.Lock()
		downloaded++
		str := bold(cyan("::") + " Downloaded PKGBUILD (%d/%d): %s\n")
		fmt.Printf(str, downloaded, len(bases), cyan(base.String()))
		mux.Unlock()
	}

	count := 0
	for k, base := range bases {
		wg.Add(1)
		go download(k, base)
		count++
		if count%25 == 0 {
			wg.Wait()
		}
	}

	wg.Wait()

	return cloned, errs.Return()
}

func downloadPkgbuildsSources(bases []Base, incompatible stringset.StringSet) (err error) {
	for _, base := range bases {
		pkg := base.Pkgbase()
		dir := filepath.Join(config.BuildDir, pkg)
		args := []string{"--verifysource", "-Ccf"}

		if incompatible.Get(pkg) {
			args = append(args, "--ignorearch")
		}

		err = show(passToMakepkg(dir, args...))
		if err != nil {
			return fmt.Errorf("Error downloading sources: %s", cyan(base.String()))
		}
	}

	return
}

func buildInstallPkgbuilds(dp *depPool, do *depOrder, srcinfos map[string]*gosrc.Srcinfo, parser *arguments, incompatible stringset.StringSet, conflicts stringset.MapStringSet) error {
	arguments := parser.copy()
	arguments.clearTargets()
	arguments.op = "U"
	arguments.delArg("confirm")
	arguments.delArg("noconfirm")
	arguments.delArg("c", "clean")
	arguments.delArg("q", "quiet")
	arguments.delArg("q", "quiet")
	arguments.delArg("y", "refresh")
	arguments.delArg("u", "sysupgrade")
	arguments.delArg("w", "downloadonly")

	deps := make([]string, 0)
	exp := make([]string, 0)
	oldConfirm := config.NoConfirm
	config.NoConfirm = true

	//remotenames: names of all non repo packages on the system
	_, _, localNames, remoteNames, err := filterPackages()
	if err != nil {
		return err
	}

	//cache as a stringset. maybe make it return a string set in the first
	//place
	remoteNamesCache := stringset.FromSlice(remoteNames)
	localNamesCache := stringset.FromSlice(localNames)

	doInstall := func() error {
		if len(arguments.targets) == 0 {
			return nil
		}

		err := show(passToPacman(arguments))
		if err != nil {
			return err
		}

		err = saveVCSInfo()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

		if err = asdeps(parser, deps); err != nil {
			return err
		}
		if err = asexp(parser, exp); err != nil {
			return err
		}

		config.NoConfirm = oldConfirm

		arguments.clearTargets()
		deps = make([]string, 0)
		exp = make([]string, 0)
		config.NoConfirm = true
		return nil
	}

	for _, base := range do.Aur {
		var err error
		pkg := base.Pkgbase()
		dir := filepath.Join(config.BuildDir, pkg)
		built := true

		satisfied := true
	all:
		for _, pkg := range base {
			for _, deps := range [3][]string{pkg.Depends, pkg.MakeDepends, pkg.CheckDepends} {
				for _, dep := range deps {
					if _, err := dp.LocalDB.PkgCache().FindSatisfier(dep); err != nil {
						satisfied = false
						fmt.Printf("%s not satisfied, flushing install queue\n", dep)
						break all
					}
				}
			}
		}

		if !satisfied || !config.BatchInstall {
			err = doInstall()
			if err != nil {
				return err
			}
		}

		srcinfo := srcinfos[pkg]

		args := []string{"--nobuild", "-fC"}

		if incompatible.Get(pkg) {
			args = append(args, "--ignorearch")
		}

		//pkgver bump
		err = show(passToMakepkg(dir, args...))
		if err != nil {
			return fmt.Errorf("Error making: %s", base.String())
		}

		pkgdests, version, err := parsePackageList(dir)
		if err != nil {
			return err
		}

		isExplicit := false
		for _, b := range base {
			isExplicit = isExplicit || dp.Explicit.Get(b.Name)
		}
		if config.ReBuild == "no" || (config.ReBuild == "yes" && !isExplicit) {
			for _, split := range base {
				pkgdest, ok := pkgdests[split.Name]
				if !ok {
					return fmt.Errorf("Could not find PKGDEST for: %s", split.Name)
				}

				_, err := os.Stat(pkgdest)
				if os.IsNotExist(err) {
					built = false
				} else if err != nil {
					return err
				}
			}
		} else {
			built = false
		}

		if cmdArgs.existsArg("needed") {
			installed := true
			for _, split := range base {
				if alpmpkg := dp.LocalDB.Pkg(split.Name); alpmpkg == nil || alpmpkg.Version() != version {
					installed = false
				}
			}

			if installed {
				err = show(passToMakepkg(dir, "-c", "--nobuild", "--noextract", "--ignorearch"))
				if err != nil {
					return fmt.Errorf("Error making: %s", err)
				}

				fmt.Println(cyan(pkg+"-"+version) + bold(" is up to date -- skipping"))
				continue
			}
		}

		if built {
			err = show(passToMakepkg(dir, "-c", "--nobuild", "--noextract", "--ignorearch"))
			if err != nil {
				return fmt.Errorf("Error making: %s", err)
			}

			fmt.Println(bold(yellow(arrow)),
				cyan(pkg+"-"+version)+bold(" already made -- skipping build"))
		} else {
			args := []string{"-cf", "--noconfirm", "--noextract", "--noprepare", "--holdver"}

			if incompatible.Get(pkg) {
				args = append(args, "--ignorearch")
			}

			err := show(passToMakepkg(dir, args...))
			if err != nil {
				return fmt.Errorf("Error making: %s", base.String())
			}
		}

		//conflicts have been checked so answer y for them
		if config.UseAsk {
			ask, _ := strconv.Atoi(cmdArgs.globals["ask"])
			uask := alpm.QuestionType(ask) | alpm.QuestionTypeConflictPkg
			cmdArgs.globals["ask"] = fmt.Sprint(uask)
		} else {
			for _, split := range base {
				if _, ok := conflicts[split.Name]; ok {
					config.NoConfirm = false
					break
				}
			}
		}

		for _, split := range base {
			pkgdest, ok := pkgdests[split.Name]
			if !ok {
				return fmt.Errorf("Could not find PKGDEST for: %s", split.Name)
			}

			arguments.addTarget(pkgdest)
			if parser.existsArg("asdeps", "asdep") {
				deps = append(deps, split.Name)
			} else if parser.existsArg("asexplicit", "asexp") {
				exp = append(exp, split.Name)
			} else if !dp.Explicit.Get(split.Name) && !localNamesCache.Get(split.Name) && !remoteNamesCache.Get(split.Name) {
				deps = append(deps, split.Name)
			}
		}

		var mux sync.Mutex
		var wg sync.WaitGroup
		for _, pkg := range base {
			wg.Add(1)
			go updateVCSData(pkg.Name, srcinfo.Source, &mux, &wg)
		}

		wg.Wait()
	}

	err = doInstall()
	config.NoConfirm = oldConfirm
	return err
}
