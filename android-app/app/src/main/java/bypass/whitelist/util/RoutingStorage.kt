package bypass.whitelist.util

import android.content.Context
import java.io.File

object RoutingStorage {
    fun defaultLink(context: Context): String =
        context.assets.open("default-routing.txt").bufferedReader().use { it.readText().trim() }

    fun profileLink(json: String): String = "happ://routing/add/" +
        android.util.Base64.encodeToString(json.toByteArray(Charsets.UTF_8), android.util.Base64.NO_WRAP)

    fun directory(context: Context): String = File(context.noBackupFilesDir, "routing").absolutePath
}
