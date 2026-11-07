package pluginruntime

// DocumentWorkshopID is the installed document-tool package identity.
const DocumentWorkshopID = "document-workshop"

// SafeDocumentResultCode validates a tool error before it is surfaced to users.
func SafeDocumentResultCode(value string) bool {
	if len(value) == 0 || len(value) > 96 {
		return false
	}
	for _, item := range value {
		if !(item >= 'a' && item <= 'z' || item >= '0' && item <= '9' || item == '_') {
			return false
		}
	}
	return true
}
