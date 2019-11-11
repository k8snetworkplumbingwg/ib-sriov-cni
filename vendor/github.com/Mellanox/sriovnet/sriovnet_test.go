package sriovnet

import (
	"fmt"
	"testing"
)

func TestEnableSriov(t *testing.T) {

	err := EnableSriov("ib0")
	if err != nil {
		t.Fatal(err)
	}
}

func TestDisableSriov(t *testing.T) {
	err := DisableSriov("ib0")
	if err != nil {
		t.Fatal(err)
	}
}

func TestGetPfHandle(t *testing.T) {
	err1 := EnableSriov("ib0")
	if err1 != nil {
		t.Fatal(err1)
	}

	handle, err2 := GetPfNetdevHandle("ib0")
	if err2 != nil {
		t.Fatal(err2)
	}
	for _, vf := range handle.List {
		fmt.Printf("vf = %v\n", vf)
	}
}

func TestConfigVfs(t *testing.T) {
	err1 := EnableSriov("ens2f0")
	if err1 != nil {
		t.Fatal(err1)
	}

	handle, err2 := GetPfNetdevHandle("ens2f0")
	if err2 != nil {
		t.Fatal(err2)
	}
	err3 := ConfigVfs(handle, false)
	if err3 != nil {
		t.Fatal(err3)
	}
	for _, vf := range handle.List {
		fmt.Printf("after config vf = %v\n", vf)
	}
}

func TestIsSriovEnabled(t *testing.T) {
	status := IsSriovEnabled("ens2f0")

	fmt.Printf("sriov status = %v", status)
}

func TestAllocFreeVf(t *testing.T) {
	var vfList [10]*VfObj

	err1 := EnableSriov("ib0")
	if err1 != nil {
		t.Fatal(err1)
	}

	handle, err2 := GetPfNetdevHandle("ib0")
	if err2 != nil {
		t.Fatal(err2)
	}
	err3 := ConfigVfs(handle, false)
	if err3 != nil {
		t.Fatal(err3)
	}
	for i := 0; i < 10; i++ {
		vfList[i], _ = AllocateVf(handle)
	}
	for _, vf := range handle.List {
		fmt.Printf("after allocation vf = %v\n", vf)
	}
	for i := 0; i < 10; i++ {
		if vfList[i] == nil {
			continue
		}
		FreeVf(handle, vfList[i])
	}
	for _, vf := range handle.List {
		fmt.Printf("after free vf = %v\n", vf)
	}
}

func TestFreeByName(t *testing.T) {
	var vfList [10]*VfObj

	err1 := EnableSriov("ib0")
	if err1 != nil {
		t.Fatal(err1)
	}

	handle, err2 := GetPfNetdevHandle("ib0")
	if err2 != nil {
		t.Fatal(err2)
	}
	err3 := ConfigVfs(handle, false)
	if err3 != nil {
		t.Fatal(err3)
	}
	for i := 0; i < 10; i++ {
		vfList[i], _ = AllocateVf(handle)
	}
	for _, vf := range handle.List {
		fmt.Printf("after allocation vf = %v\n", vf)
	}
	for i := 0; i < 10; i++ {
		if vfList[i] == nil {
			continue
		}
		FreeVfByNetdevName(handle, vfList[i].NetdevName)
	}
	for _, vf := range handle.List {
		fmt.Printf("after free vf = %v\n", vf)
	}
}

func TestAllocateVfByMac(t *testing.T) {
	var vfList [10]*VfObj
	var vfName [10]string

	err1 := EnableSriov("ens2f1")
	if err1 != nil {
		t.Fatal(err1)
	}

	handle, err2 := GetPfNetdevHandle("ens2f1")
	if err2 != nil {
		t.Fatal(err2)
	}
	err3 := ConfigVfs(handle, true)
	if err3 != nil {
		t.Fatal(err3)
	}
	for i := 0; i < 10; i++ {
		vfList[i], _ = AllocateVf(handle)
		if vfList[i] != nil {
			vfName[i] = vfList[i].NetdevName
		}
	}
	for _, vf := range handle.List {
		fmt.Printf("after allocation vf = %v\n", vf)
	}
	for i := 0; i < 10; i++ {
		if vfList[i] == nil {
			continue
		}
		FreeVf(handle, vfList[i])
	}
	for _, vf := range handle.List {
		fmt.Printf("after alloc vf = %v\n", vf)
	}
	for i := 0; i < 2; i++ {
		if vfName[i] == "" {
			continue
		}
		mac, _ := GetVfDefaultMacAddr(vfName[i])
		vfList[i], _ = AllocateVfByMacAddress(handle, mac)
	}
	for _, vf := range handle.List {
		fmt.Printf("after alloc vf = %v\n", vf)
	}
}

func TestGetVfPciDevList(t *testing.T) {

	list, _ := GetVfPciDevList("ens2f1")
	fmt.Println("list is: ", list)
	t.Fatal(nil)
}
