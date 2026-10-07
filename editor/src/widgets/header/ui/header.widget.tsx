import { ActionIcon, Group, Text, Title } from '@mantine/core'
import { StickyHeader } from '@/shared/ui/sticky-header'
import { PiGithubLogo, PiStar } from 'react-icons/pi'

import classes from './header.module.css'

export function HeaderWidget() {
    return (
        <StickyHeader className={classes.root} px="md">
            <Group h="100%" justify="space-between">
                <Title order={3}>Xray Config Monaco Editor</Title>

                <ActionIcon
                    component="a"
                    href="https://github.com/remnawave/xray-monaco-editor"
                    size="lg"
                    target="_blank"
                    variant="subtle"
                >
                    <PiGithubLogo size={24} />
                </ActionIcon>
            </Group>

            <Group gap="xs">
                <Text c="dimmed" size="md">
                    Forked from{' '}
                    <Text component="a" href="https://github.com/mmmray/xray-online" inherit>
                        @mmmray
                    </Text>
                </Text>

                <ActionIcon
                    component="a"
                    href="https://github.com/mmmray/xray-online"
                    size="lg"
                    target="_blank"
                    variant="subtle"
                >
                    <PiStar size={24} />
                </ActionIcon>
            </Group>
        </StickyHeader>
    )
}
