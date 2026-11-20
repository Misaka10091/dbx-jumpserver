//go:build darwin && cgo

#import <Cocoa/Cocoa.h>
#include <string.h>

char *dbx_show_captcha(const void *image, size_t len, int *status) {
    @autoreleasepool {
        NSImage *picture = [[NSImage alloc] initWithData:[NSData dataWithBytes:image length:len]];
        if (picture == nil || !picture.isValid) {
            *status = 4;
            return NULL;
        }

        [NSApplication sharedApplication];
        [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];

        NSAlert *alert = [[NSAlert alloc] init];
        alert.messageText = @"JumpServer 图形验证码";
        alert.informativeText = @"请输入下图中的字符。";
        [alert addButtonWithTitle:@"提交"];
        [alert addButtonWithTitle:@"取消"];

        NSView *panel = [[NSView alloc] initWithFrame:NSMakeRect(0, 0, 380, 126)];
        NSImageView *preview = [[NSImageView alloc] initWithFrame:NSMakeRect(0, 43, 380, 81)];
        preview.image = picture;
        preview.imageScaling = NSImageScaleProportionallyDown;
        preview.imageAlignment = NSImageAlignCenter;
        preview.accessibilityLabel = @"JumpServer 验证码图片";
        [panel addSubview:preview];

        NSTextField *input = [[NSTextField alloc] initWithFrame:NSMakeRect(0, 3, 380, 28)];
        input.placeholderString = @"输入验证码";
        input.accessibilityLabel = @"输入验证码";
        [panel addSubview:input];
        alert.accessoryView = panel;
        alert.window.initialFirstResponder = input;
        [alert.window makeFirstResponder:input];
        [NSApp activateIgnoringOtherApps:YES];
        NSModalResponse response = [alert runModal];
        char *answer = NULL;
        if (response == NSAlertFirstButtonReturn) {
            answer = strdup(input.stringValue.UTF8String ?: "");
            *status = 0;
        } else {
            *status = 3;
        }
        input.stringValue = @"";
        preview.image = nil;
        alert.accessoryView = nil;
        [alert.window orderOut:nil];
        [input release];
        [preview release];
        [panel release];
        [alert release];
        [picture release];
        return answer;
    }
}
