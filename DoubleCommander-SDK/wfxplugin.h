#ifndef WFXS3_DOUBLE_COMMANDER_WFXPLUGIN_H
#define WFXS3_DOUBLE_COMMANDER_WFXPLUGIN_H

#include <stdint.h>

#define MAX_PATH 260
#define FILE_ATTRIBUTE_NORMAL 0x00000080
#define FILE_ATTRIBUTE_DIRECTORY 0x00000010

typedef uint32_t DWORD;
typedef uint16_t WCHAR;
typedef int BOOL;

#pragma pack(push, 1)
typedef struct {
	DWORD dwLowDateTime;
	DWORD dwHighDateTime;
} FILETIME;

typedef struct {
	DWORD dwFileAttributes;
	FILETIME ftCreationTime;
	FILETIME ftLastAccessTime;
	FILETIME ftLastWriteTime;
	DWORD nFileSizeHigh;
	DWORD nFileSizeLow;
	DWORD dwReserved0;
	DWORD dwReserved1;
	WCHAR cFileName[MAX_PATH];
	WCHAR cAlternateFileName[14];
} WIN32_FIND_DATAW;
#pragma pack(pop)

typedef struct {
	DWORD SizeLow, SizeHigh;
	FILETIME LastWriteTime;
	int Attr;
} RemoteInfoStruct;

typedef struct {
	int size;
	DWORD PluginInterfaceVersionLow;
	DWORD PluginInterfaceVersionHi;
	char DefaultIniName[MAX_PATH];
} FsDefaultParamStruct;

typedef int (*tProgressProcW)(int PluginNr, WCHAR* SourceName,
	WCHAR* TargetName, int PercentDone);
typedef void (*tLogProcW)(int PluginNr, int MsgType, WCHAR* LogString);
typedef BOOL (*tRequestProcW)(int PluginNr, int RequestType, WCHAR* CustomTitle,
	WCHAR* CustomText, WCHAR* ReturnedText, int maxlen);

#endif
